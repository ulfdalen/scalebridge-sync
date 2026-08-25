package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// openBrowser points the default browser at url and returns as soon as the
// helper is launched.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		// rundll32 takes the URL as one plain argument; `cmd /c start` goes
		// through the shell, which eats & in a URL.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		// Headless box (a Pi, a NAS, SSH): no browser to open, and that is not
		// an error worth failing a command over.
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			fmt.Printf("no desktop session detected - open %s in a browser yourself\n", url)
			return nil
		}
		return exec.Command("xdg-open", url).Start()
	}
}
