//go:build darwin || linux

package main

import "os"

func isElevated() bool { return os.Geteuid() == 0 }

// prepareWorkDir creates the working folder and returns it (callers must write
// only to the returned folder). As root it lives under a root-owned parent
// (/var/cache, /Library/Application Support) that other users cannot write to.
func prepareWorkDir() (dir, note string, err error) {
	dir = workDir()
	return dir, "", os.MkdirAll(dir, 0o700)
}

// detectInteractive reports whether stdin is a terminal.
func detectInteractive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func canShowUI() bool { return true }

func platformUpArgs() []string { return nil }
