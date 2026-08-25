package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kardianos/service"

	"github.com/ulfdalen/scalebridge-sync/internal/store"
)

// newService builds the OS service handle. dir must already be absolute.
func newService(dir string) (service.Service, error) {
	cfg := &service.Config{
		Name:        "scalebridge-sync",
		DisplayName: "ScaleBridge Sync",
		Description: "Syncs Withings scale measurements to Garmin Connect.",
		// The absolute config dir is baked in at install time: on Windows the
		// service runs as LocalSystem, whose os.UserConfigDir() is not the
		// installing user's AppData, so it would sync from an empty state file.
		Arguments: []string{"run", "--config", dir},
	}

	switch runtime.GOOS {
	case "darwin":
		// LaunchAgent in ~/Library/LaunchAgents: runs as the user, at login.
		// RunAtLoad is off by default in kardianos, so set it explicitly rather
		// than rely on KeepAlive to start the job.
		cfg.Option = service.KeyValue{"UserService": true, "RunAtLoad": true}
	case "linux":
		// Only systemd supports user units; sysv/upstart/openrc reject the option
		// outright, so there we fall back to a system-level unit.
		if service.Platform() == "linux-systemd" {
			cfg.Option = service.KeyValue{"UserService": true}
		}
	}

	return service.New(&program{dir: dir}, cfg)
}

// program is the kardianos side of the same server "run" starts.
type program struct {
	dir string
	app *app
}

func (p *program) Start(s service.Service) error {
	a, err := newApp(p.dir, "127.0.0.1")
	if err != nil {
		return err
	}
	if err := a.listen(); err != nil {
		return fmt.Errorf("listen on 127.0.0.1:%d: %w", a.port, err)
	}
	p.app = a
	a.start() // must return promptly: the serving happens in goroutines
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.app != nil {
		p.app.stop()
	}
	return nil
}

func runUnderServiceManager(dir string) int {
	svc, err := newService(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := svc.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// ── control commands ──

func cmdInstall(dir string) int {
	// Create the config dir first: the absolute path baked into the unit has to
	// exist before the service ever starts.
	if _, err := store.Open(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	svc, err := newService(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := svc.Install(); err != nil {
		fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
		if runtime.GOOS == "windows" {
			fmt.Fprintln(os.Stderr, "registering a Windows Service needs an Administrator command prompt")
		}
		if runtime.GOOS == "linux" && service.Platform() != "linux-systemd" {
			fmt.Fprintf(os.Stderr, "on %s the unit is system-level; try again with sudo\n", service.Platform())
		}
		return 1
	}

	switch {
	case runtime.GOOS == "darwin":
		fmt.Println("Installed as a LaunchAgent in ~/Library/LaunchAgents - it starts at login.")
	case runtime.GOOS == "windows":
		fmt.Println("Installed as a Windows Service (LocalSystem) - it starts at boot.")
		fmt.Printf("Config dir baked into the service: %s\n", dir)
	case service.Platform() == "linux-systemd":
		fixUserUnitTarget()
		fmt.Println("Installed as a systemd user unit - it starts when you log in.")
		fmt.Println("loginctl enable-linger $USER  # keeps it running when you log out")
	default:
		fmt.Printf("Installed as a %s service.\n", service.Platform())
	}
	fmt.Println("Start it now with: scalebridge-sync start")
	return 0
}

func cmdUninstall(dir string) int {
	svc, err := newService(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	svc.Stop() // it may not be running; the uninstall is what matters
	if err := svc.Uninstall(); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall failed: %v\n", err)
		return 1
	}
	fmt.Println("Service removed. Your configuration and tokens are untouched:")
	fmt.Println("  " + dir)
	return 0
}

func cmdControl(dir, action string) int {
	svc, err := newService(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Ask first: launchctl's failure for a missing plist is "Load failed: 5:
	// Input/output error", which tells the user nothing.
	if _, serr := svc.Status(); notInstalled(serr) {
		fmt.Fprintln(os.Stderr, "the service is not installed - run: scalebridge-sync install")
		return 1
	}

	if action == "start" {
		err = svc.Start()
	} else {
		err = svc.Stop()
	}
	if err != nil {
		if notInstalled(err) {
			fmt.Fprintln(os.Stderr, "the service is not installed - run: scalebridge-sync install")
			return 1
		}
		fmt.Fprintf(os.Stderr, "%s failed: %v\n", action, err)
		return 1
	}
	fmt.Printf("Service %sed.\n", action)
	return 0
}

func cmdStatus(dir string) int {
	st, port, err := openStore(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	svc, err := newService(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	switch status, err := svc.Status(); {
	case notInstalled(err):
		fmt.Println("service:  not installed")
	case err != nil:
		fmt.Printf("service:  unknown (%v)\n", err)
	case status == service.StatusRunning:
		fmt.Println("service:  running")
	case status == service.StatusStopped:
		fmt.Println("service:  stopped")
	default:
		fmt.Println("service:  unknown")
	}

	if dialable(port) {
		fmt.Printf("web UI:   answering at %s\n", localURL(port))
	} else {
		fmt.Printf("web UI:   not answering on port %d\n", port)
	}
	fmt.Printf("config:   %s\n", st.FilePath())
	return 0
}

// Repoints the freshly installed unit at default.target: kardianos hardcodes
// WantedBy=multi-user.target, which only exists in the system manager, so as a
// user unit it enables cleanly and then never starts at login. Best effort - a
// failure here costs the login start, not the install.
func fixUserUnitTarget() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	unit := filepath.Join(home, ".config/systemd/user/scalebridge-sync.service")
	body, err := os.ReadFile(unit)
	if err != nil {
		return
	}
	fixed := strings.Replace(string(body), "WantedBy=multi-user.target", "WantedBy=default.target", 1)
	if fixed == string(body) {
		return
	}
	if err := os.WriteFile(unit, []byte(fixed), 0o644); err != nil {
		return
	}
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	// reenable = disable + enable: drops the stale multi-user.target.wants link.
	exec.Command("systemctl", "--user", "reenable", "scalebridge-sync.service").Run()
}

// Covers the systems that return ErrNotInstalled and the ones (launchd, some
// systemd versions) that only say so in the error text.
func notInstalled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, service.ErrNotInstalled) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "not installed")
}
