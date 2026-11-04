//go:build !windows

package main

import (
	"context"
	"log"
	"os/exec"
	"runtime"
)

// openWindow opens the chat as an app window in Chrome, Chromium or Edge
// when there is one (closing it ends the program), else in the default
// browser (the program runs until Ctrl+C).
func openWindow(ctx context.Context, url, profile string) <-chan struct{} {
	closed := make(chan struct{})
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, path, "--app="+url, "--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check")
		if err := cmd.Start(); err != nil {
			continue
		}
		go func() {
			_ = cmd.Wait()
			close(closed)
		}()
		return closed
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, url).Start(); err != nil {
		log.Printf("open %s yourself: %v", url, err)
	}
	return closed
}

func isConsoleWindow() bool { return false }
