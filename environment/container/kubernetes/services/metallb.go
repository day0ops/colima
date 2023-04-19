package services

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
	"time"

	"github.com/abiosoft/colima/cli"
	"github.com/abiosoft/colima/embedded"
	"github.com/abiosoft/colima/environment"
	"github.com/abiosoft/colima/util/downloader"
)

const metallbVersion = "v0.13.9"

func InstallMetallb(
	host environment.HostActions,
	guest environment.GuestActions,
	a *cli.ActiveCommandChain,
	cidrBlock string,
) {
	metallbConfigPath := "/tmp/metallb-config.yaml"

	downloadPath := "/tmp/metallb-native.yaml"
	url := "https://raw.githubusercontent.com/metallb/metallb/" + metallbVersion + "/config/manifests/metallb-native.yaml"
	a.Stage("installing MetalLB")
	a.Retry("", time.Second*5, 30, func(retryCount int) error {
		return downloader.Download(host, guest, url, downloadPath)
	})
	a.Retry("", time.Second*5, 30, func(retryCount int) error {
		return guest.Run("kubectl", "apply", "-f", downloadPath)
	})

	a.Add(func() error {
		var availableData = map[string]string{
			"IpAddressRange": cidrBlock,
		}
		install, err := embedded.ReadString("metallb/config.yaml")
		if err != nil {
			return fmt.Errorf("error reading embedded metallb config: %w", err)
		}
		tmpl, err := template.New("config.yaml").Parse(install)
		if err != nil {
			return fmt.Errorf("error parsing embedded metallb config: %w", err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, availableData); err != nil {
			return fmt.Errorf("error parsing embedded metallb config: %w", err)
		}
		return guest.Write(metallbConfigPath, buf.Bytes())
	})

	a.Retry("", time.Second*5, 30, func(retryCount int) error {
		return guest.Run("kubectl", "apply", "-f", metallbConfigPath)
	})
}
