package kubernetes

import (
	"fmt"
	"strings"

	"github.com/abiosoft/colima/cli"
	"github.com/abiosoft/colima/config"
	"github.com/abiosoft/colima/environment"
	"github.com/abiosoft/colima/environment/container/containerd"
	"github.com/abiosoft/colima/environment/container/docker"
	"github.com/abiosoft/colima/environment/container/kubernetes/services"
	"github.com/abiosoft/colima/environment/vm/lima/limautil"
	"github.com/abiosoft/colima/util/downloader"
	"github.com/sirupsen/logrus"
)

func installK3s(host environment.HostActions,
	guest environment.GuestActions,
	a *cli.ActiveCommandChain,
	log *logrus.Entry,
	containerRuntime string,
	conf config.Kubernetes,
) {
	installK3sBinary(host, guest, a, conf)
	installK3sCache(host, guest, a, log, containerRuntime, conf)
	installK3sCluster(host, guest, a, log, containerRuntime, conf)
}

func installK3sBinary(
	host environment.HostActions,
	guest environment.GuestActions,
	a *cli.ActiveCommandChain,
	conf config.Kubernetes,
) {
	// install k3s last to ensure it is the last step
	downloadPath := "/tmp/k3s"
	url := "https://github.com/k3s-io/k3s/releases/download/" + conf.Version + "/k3s"
	if guest.Arch().GoArch() == "arm64" {
		url += "-arm64"
	}
	a.Add(func() error {
		return downloader.Download(host, guest, url, downloadPath)
	})
	a.Add(func() error {
		return guest.Run("sudo", "install", downloadPath, "/usr/local/bin/k3s")
	})
}

func installK3sCache(
	host environment.HostActions,
	guest environment.GuestActions,
	a *cli.ActiveCommandChain,
	log *logrus.Entry,
	containerRuntime string,
	conf config.Kubernetes,
) {
	imageTar := "k3s-airgap-images-" + guest.Arch().GoArch() + ".tar"
	imageTarGz := imageTar + ".gz"
	downloadPathTar := "/tmp/" + imageTar
	downloadPathTarGz := "/tmp/" + imageTarGz
	url := "https://github.com/k3s-io/k3s/releases/download/" + conf.Version + "/" + imageTarGz
	a.Add(func() error {
		return downloader.Download(host, guest, url, downloadPathTarGz)
	})
	a.Add(func() error {
		return guest.Run("gzip", "-f", "-d", downloadPathTarGz)
	})

	airGapDir := "/var/lib/rancher/k3s/agent/images/"
	a.Add(func() error {
		return guest.Run("sudo", "mkdir", "-p", airGapDir)
	})
	a.Add(func() error {
		return guest.Run("sudo", "cp", downloadPathTar, airGapDir)
	})

	// load OCI images for K3s
	// this can be safely ignored if failed as the images would be pulled afterwards.
	switch containerRuntime {
	case containerd.Name:
		a.Stage("loading oci images")
		a.Add(func() error {
			if err := guest.Run("sudo", "ctr", "-n", "k8s.io", "images", "import", "--all-platforms", downloadPathTar); err != nil {
				log.Warnln(fmt.Errorf("error loading oci images: %w", err))
				log.Warnln("startup may delay a bit as images will be pulled from oci registry")
			}
			return nil
		})
	case docker.Name:
		a.Stage("loading oci images")
		a.Add(func() error {
			if err := guest.Run("sudo", "docker", "load", "-i", downloadPathTar); err != nil {
				log.Warnln(fmt.Errorf("error loading oci images: %w", err))
				log.Warnln("startup may delay a bit as images will be pulled from oci registry")
			}
			return nil
		})
	}
}

func installK3sCluster(
	host environment.HostActions,
	guest environment.GuestActions,
	a *cli.ActiveCommandChain,
	log *logrus.Entry,
	containerRuntime string,
	conf config.Kubernetes,
) {
	// install k3s last to ensure it is the last step
	downloadPath := "/tmp/k3s-install.sh"
	url := "https://raw.githubusercontent.com/k3s-io/k3s/" + conf.Version + "/install.sh"
	a.Add(func() error {
		return downloader.Download(host, guest, url, downloadPath)
	})
	a.Add(func() error {
		return guest.Run("sudo", "install", downloadPath, "/usr/local/bin/k3s-install.sh")
	})

	args := []string{
		"--write-kubeconfig-mode", "644",
		"--resolv-conf", "/etc/resolv.conf",
	}

	if !conf.Ingress {
		args = append(args, "--disable", "traefik")
	}

	if conf.ServiceLB {
		args = append(args, "--disable", "servicelb")
	}

	// replace ip address if networking is enabled
	ipAddress := limautil.IPAddress(config.CurrentProfile().ID)
	log.Info("discovered ip address ", ipAddress)
	if ipAddress == "127.0.0.1" {
		args = append(args, "--flannel-iface", "eth0")
	} else {
		args = append(args, "--bind-address", ipAddress)
		args = append(args, "--advertise-address", ipAddress)
		args = append(args, "--flannel-iface", "col0")
	}

	for key, value := range conf.NodeLabels {
		log = log.WithField(key, value)
		args = append(args, "--node-label", fmt.Sprintf("%s=%s", key, value))
	}
	log.Info("adding node labels")

	switch containerRuntime {
	case docker.Name:
		args = append(args, "--container-runtime-endpoint", "unix:///run/cri-dockerd.sock")
	case containerd.Name:
		args = append(args, "--container-runtime-endpoint", "unix:///run/containerd/containerd.sock")
	}
	a.Add(func() error {
		return guest.Run("sh", "-c", "INSTALL_K3S_SKIP_DOWNLOAD=true INSTALL_K3S_SKIP_ENABLE=true k3s-install.sh "+strings.Join(args, " "))
	})
}

func installAdditionalServices(
	host environment.HostActions,
	guest environment.GuestActions,
	a *cli.ActiveCommandChain,
	log *logrus.Entry,
	conf config.Kubernetes,
) {
	if conf.AdditionalServices.InstallMetalLB {
		services.InstallMetallb(host, guest, a, conf.AdditionalServices.MetalLBAddressPool)
	}
}
