//go:build windows

package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// openWindow opens the chat as an app window: Edge (on every Windows) or
// Chrome with --app, on a profile of its own, so the window is its own
// process and closing it ends the program. Without either, the default
// browser opens it and the program runs until Ctrl+C.
func openWindow(ctx context.Context, url, profile string) <-chan struct{} {
	closed := make(chan struct{})
	browser := findBrowser()
	if browser == "" {
		log.Print("no Edge or Chrome found: opening the default browser; close this window to disconnect")
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
		return closed
	}
	cmd := exec.CommandContext(ctx, browser, "--app="+url, "--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check", "--window-size=1200,860")
	if err := cmd.Start(); err != nil {
		log.Printf("open the window: %v", err)
		return closed
	}
	go func() {
		_ = cmd.Wait()
		close(closed)
	}()
	return closed
}

func findBrowser() string {
	var candidates []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		if base := os.Getenv(env); base != "" {
			candidates = append(candidates,
				filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
		}
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	if path, err := exec.LookPath("msedge"); err == nil {
		return path
	}
	return ""
}

// isConsoleWindow: a double-clicked .exe has a console of its own, which
// would vanish with an error before it can be read.
func isConsoleWindow() bool { return true }
