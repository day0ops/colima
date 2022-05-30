package fsnotify

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/abiosoft/colima/daemon/process"
	"github.com/abiosoft/colima/environment"
	"github.com/abiosoft/colima/environment/vm/lima/limautil"
	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
)

var (
	CtxKeyGuest = func() any { return struct{ guestKey string }{guestKey: "guest"} }
)

// Name returns the name
func Name() string { return "fsnotify" }

// New returns fsnotify process.
func New() process.Process {
	return &fsnotifyProcess{}
}

type fsnotifyProcess struct {
	guest environment.GuestActions
	dirs  []string
	alive bool
	sync.Mutex
}

// Alive implements process.Process
func (f *fsnotifyProcess) Alive(ctx context.Context) error {
	f.Lock()
	defer f.Unlock()

	if f.alive {
		return nil
	}
	return fmt.Errorf("not running")
}

// Dependencies implements process.Process
func (*fsnotifyProcess) Dependencies() (deps []process.Dependency, root bool) {
	return nil, false
}

// Name implements process.Process
func (*fsnotifyProcess) Name() string { return Name() }

// Start implements process.Process
func (f *fsnotifyProcess) Start(ctx context.Context) error {
	guest, ok := ctx.Value(CtxKeyGuest()).(environment.GuestActions)
	if !ok {
		return fmt.Errorf("environment.GuestAction missing in context")
	}
	f.guest = guest

	f.waitForLima(ctx)

	c, err := limautil.InstanceConfig()
	if err != nil {
		return fmt.Errorf("error retrieving config")
	}

	for _, mount := range c.MountsOrDefault() {
		p, err := mount.CleanPath()
		if err != nil {
			return fmt.Errorf("error retrieving mount path: %w", err)
		}
		f.dirs = append(f.dirs, strings.TrimSuffix(p, "/")) // trailing slash must be ommitted for fsnotify
	}

	return f.watch(ctx)
}

// waitForLima waits until lima starts and sets the directory to watch.
func (f *fsnotifyProcess) waitForLima(ctx context.Context) {
	// wait for Lima to finish starting
	for {
		logrus.Trace("waiting for Lima...")

		// 5 second interval
		after := time.After(time.Second * 5)

		select {
		case <-ctx.Done():
			return
		case <-after:
			i, err := limautil.Instance()
			if err == nil && i.Running() {
				return
			}
		}
	}
}

func traverseDir(watcher *fsnotify.Watcher, parent, dir string) error {
	current := filepath.Join(parent, dir)
	children, err := os.ReadDir(current)
	if err != nil {
		logrus.Error(fmt.Errorf("error retrieving dirlist for '%s': %w", current, err))
		return nil
	}

	// add root
	if err := watcher.Add(current); err != nil {
		logrus.Trace(fmt.Errorf("fsnotify: error adding '%s' to watch directories: %w", current, err))
	} else {
		logrus.Tracef("fsnotify: added %s to watch directories", current)
	}

	// traverse children
	for _, child := range children {
		// only treat directories
		if !child.IsDir() {
			continue
		}
		// skip hidden files
		if strings.HasPrefix(child.Name(), ".") {
			logrus.Trace(fmt.Errorf("skipping hidden child directory '%s' of '%s'", child.Name(), current))
			continue
		}

		if perm := child.Type().Perm(); perm&fs.ModeSymlink == fs.ModeSymlink {
			logrus.Trace(fmt.Errorf("skipping symlink directory '%s' of '%s'", child.Name(), current))
			continue
		}

		err := traverseDir(watcher, current, child.Name())
		if err != nil {
			return fmt.Errorf("error traversing child directory '%s' of '%s': %w", child.Name(), current, err)
		}
	}
	return nil
}

func (f *fsnotifyProcess) watch(ctx context.Context) error {
	// start watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("error creating watcher: %w", err)
	}
	defer watcher.Close()

	// traverse directory and add to watch list
	for _, dir := range f.dirs {
		root := os.DirFS(dir)
		err := fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				logrus.Error(fmt.Errorf("error in walkdir for '%s': %w", path, err))
			}
			// skip all hidden files/folders
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
				logrus.Tracef("fsnotify: skipped hidden dir '%s'", path)
				return filepath.SkipDir
			}

			if d.IsDir() {
				if err := watcher.Add(path); err != nil {
					logrus.Errorf("fsnotify: error adding '%s' to watch directories: %v", path, err)
					return nil
				}
				logrus.Tracef("fsnotify: added %s to watch directories", path)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("error in directory walk: %w", err)
		}

	}

	f.Lock()
	f.alive = true
	f.Unlock()

	// accumulate events per second and dispatch in batch
	for {
		var events []fsnotify.Event
		after := time.After(time.Second * 1)

	loop:
		for {
			select {

			case ev, ok := <-watcher.Events:
				if !ok {
					return fmt.Errorf("watcher channel closed")
				}
				logrus.Tracef("fsnotify: got event: %s, file: %s", ev.Op, ev.Name)

				// if write event
				if ev.Op&fsnotify.Write == fsnotify.Write {
					events = append(events, ev)
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return fmt.Errorf("watcher channel closed")
				}
				logrus.Tracef("fsnotify: watch error: %v", err)

			case <-after:
				go f.Dispatch(events)
				break loop

			case <-ctx.Done():
				return nil
			}

		}
	}

}

func (f *fsnotifyProcess) Dispatch(events []fsnotify.Event) {
	l := len(events)

	switch {

	// nothing to do
	case l == 0:
		return

	// at most 10 events, discard the rest
	case l > 10:
		logrus.Tracef("fsnotify events more than 10 (%d), discarding the extra %d", l, l-10)
		events = events[:10]
	}

	// dispatch in parallel
	for _, ev := range events {
		logrus.Tracef("%s modified, touching...", ev.Name)
		go func(ev fsnotify.Event) {
			f.Touch(ev.Name)
		}(ev)
	}
}

func (f *fsnotifyProcess) Touch(file string) error {
	return f.guest.RunQuiet("touch", file)
}
