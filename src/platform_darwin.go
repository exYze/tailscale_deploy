//go:build darwin

package main

// macOS: installs the official Developer ID-signed "Standalone" Tailscale app
// (.pkg from pkgs.tailscale.com). macOS always requires the VPN configuration
// and system extension to be approved, either by a user clicking "Allow"
// or, with zero clicks, by an MDM-pushed profile (see
// Tailscale-macOS-MDM.mobileconfig).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	notReadyExitCode = exitPending
	appPath          = "/Applications/Tailscale.app"
	appCLI           = appPath + "/Contents/MacOS/Tailscale"
	tailscaleTeamID  = "W5364U7YZB" // Tailscale Inc.'s Apple Developer Team ID
)

var macMajor int

func platformName() string { return "darwin/" + runtime.GOARCH }

func tailscaleCLIPath() (string, bool) { return appCLI, fileExists(appCLI) }

func workDir() string {
	if isElevated() {
		return "/Library/Application Support/TailscaleDeploy"
	}
	return filepath.Join(os.TempDir(), "TailscaleDeploy")
}

func logFilePath() string { return "/Library/Logs/TailscaleDeploy.log" }

func checkOSSupported() (string, error) {
	out, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	ver := strings.TrimSpace(string(out))
	if err != nil || ver == "" {
		return "macOS (unknown version)", nil
	}
	macMajor = parseMajor(ver)
	cpu := "Intel"
	if b, _ := exec.Command("/usr/sbin/sysctl", "-n", "hw.optional.arm64").Output(); strings.TrimSpace(string(b)) == "1" {
		cpu = "Apple silicon"
	}
	desc := fmt.Sprintf("macOS %s (%s)", ver, cpu)
	if macMajor > 0 && macMajor < 13 {
		return desc, fmt.Errorf("%s is not supported: the current Tailscale app requires macOS 13 Ventura or newer", desc)
	}
	return desc, nil
}

// consoleUser returns the user logged in at the Mac's screen, if any.
func consoleUser() (name, uid string, ok bool) {
	out, err := exec.Command("/usr/bin/stat", "-f", "%Su", "/dev/console").Output()
	name = strings.TrimSpace(string(out))
	if err != nil || name == "" || name == "root" || name == "loginwindow" || name == "_mbsetupuser" {
		return "", "", false
	}
	out, err = exec.Command("/usr/bin/id", "-u", name).Output()
	if err != nil {
		return "", "", false
	}
	return name, strings.TrimSpace(string(out)), true
}

// asConsoleUser builds a command that runs argv in the console user's GUI session.
func asConsoleUser(name, uid string, argv ...string) (string, []string) {
	return "/bin/launchctl", append([]string{"asuser", uid, "/usr/bin/sudo", "-u", name}, argv...)
}

// runCLI runs the Tailscale CLI as root and, if that fails, as the console
// user (the GUI app's local API is owned by that user's session).
//
// The temporary key file stays readable by root only: the console user may be
// a standard user, who must not be able to copy the key (an OAuth client
// secret never expires). In the console user's session the CLI gets the key
// over stdin instead.
func runCLI(ctx context.Context, timeout time.Duration, quiet bool, cli string, args ...string) (string, error) {
	out, err := runCmd(ctx, timeout, quiet, cli, args...)
	if err == nil || !isElevated() {
		return out, err
	}
	name, uid, ok := consoleUser()
	if !ok {
		return out, err
	}
	userArgs, keyPath := keyFileToStdin(args)
	var stdin io.Reader
	if keyPath != "" {
		key, rerr := os.ReadFile(keyPath)
		if rerr != nil {
			return out, err
		}
		stdin = bytes.NewReader(key)
	}
	bin, full := asConsoleUser(name, uid, append([]string{cli}, userArgs...)...)
	out2, err2 := runCmdWith(ctx, timeout, quiet, func(c *exec.Cmd) { c.Stdin = stdin }, bin, full...)
	if err2 == nil {
		return out2, nil
	}
	return out, err
}

func installTailscale(ctx context.Context, cfg *Config, dryRun bool) error {
	ver, err := resolveVersion(ctx, cfg, func(ix *pkgIndex) string { return ix.MacZipsVersion })
	if err != nil {
		return err
	}
	name := fmt.Sprintf("Tailscale-%s-macos.pkg", ver)
	dir := workDir()
	if _, err := prepareWorkDir(); err != nil {
		return err
	}
	pkg := filepath.Join(dir, name)
	if err := downloadVerified(ctx, cfg.baseURL()+"/"+name, pkg); err != nil {
		return err
	}
	out, err := runCmd(ctx, 2*time.Minute, true, "/usr/sbin/pkgutil", "--check-signature", pkg)
	if err != nil || !strings.Contains(out, tailscaleTeamID) {
		return fmt.Errorf("installer package signature check failed:\n%s", out)
	}
	logf("Installer signature OK (Developer ID Installer: Tailscale Inc., %s)", tailscaleTeamID)
	if dryRun {
		logf("Would run: installer -pkg %q -target /", pkg)
		return nil
	}
	logf("Installing Tailscale %s...", ver)
	_, err = runCmd(ctx, 20*time.Minute, false, "/usr/sbin/installer", "-pkg", pkg, "-target", "/")
	return err
}

func ensureDaemonRunning(ctx context.Context) error {
	name, uid, ok := consoleUser()
	if !ok {
		return errors.New("no user is logged in to this Mac, so the Tailscale app cannot be started yet")
	}
	bin, args := asConsoleUser(name, uid, "/usr/bin/open", "-a", appPath)
	_, err := runCmd(ctx, time.Minute, true, bin, args...)
	return err
}

func onDaemonSlow(ctx context.Context) {
	logf("Tailscale is waiting for macOS approval. On this Mac:")
	logf("  1. If asked \"Tailscale would like to add VPN configurations\", click Allow.")
	pane := "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension"
	if macMajor >= 15 {
		pane = "x-apple.systempreferences:com.apple.LoginItems-Settings.extension"
		logf("  2. System Settings > General > Login Items & Extensions > Network Extensions: turn on Tailscale.")
	} else {
		logf("  2. System Settings > Privacy & Security: click Allow next to the message about Tailscale.")
	}
	logf("  (To avoid these clicks on every Mac, push Tailscale-macOS-MDM.mobileconfig from your MDM first.)")
	if name, uid, ok := consoleUser(); ok {
		bin, args := asConsoleUser(name, uid, "/usr/bin/open", pane)
		runCmd(ctx, 30*time.Second, true, bin, args...)
	}
}

func notReadyHint() string {
	return "Tailscale is installed, but macOS has not approved it yet (VPN configuration / system extension), " +
		"or no user is logged in. Approve Tailscale on this Mac, or push Tailscale-macOS-MDM.mobileconfig from your MDM, then run this again."
}

func notifyUser(ok bool, msg string) {
	if isElevated() {
		return // root has no GUI session; the non-root launcher shows the dialog
	}
	macDialog(ok, msg)
}

func macDialog(ok bool, msg string) {
	if os.Getenv("SSH_CONNECTION") != "" {
		return
	}
	icon := "note"
	if !ok {
		icon = "stop"
	}
	script := "display dialog " + appleScriptString(msg) + ` with title "Tailscale Deploy" buttons {"OK"} default button "OK" with icon ` + icon
	exec.Command("/usr/bin/osascript", "-e", script).Run()
}

// relaunchElevated re-runs this program as root through the standard macOS
// administrator password dialog, streaming the root copy's log to this window.
func relaunchElevated(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot find own executable:", err)
		return exitFailed
	}
	if r, e := filepath.EvalSymlinks(exe); e == nil {
		exe = r
	}
	parts := []string{shellQuote(exe)}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	parts = append(parts, "--silent")
	shellCmd := strings.Join(parts, " ") + " >/dev/null 2>&1"
	script := "do shell script " + appleScriptString(shellCmd) +
		" with prompt " + appleScriptString("Tailscale Deploy needs administrator access to install Tailscale and connect this Mac.") +
		" with administrator privileges"

	logPath := logFilePath()
	var offset int64
	if fi, err := os.Stat(logPath); err == nil {
		offset = fi.Size()
	}
	fmt.Println("Requesting administrator access...")

	var stderr bytes.Buffer
	cmd := exec.Command("/usr/bin/osascript", "-e", script)
	cmd.Stderr = &stderr
	done := make(chan struct{})
	var tailed strings.Builder
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tailFile(logPath, offset, done, func(line string) {
			fmt.Println(line)
			tailed.WriteString(line + "\n")
		})
	}()
	runErr := cmd.Run()
	close(done)
	wg.Wait()

	code := exitOK
	if runErr != nil {
		code = osascriptExitCode(stderr.String())
	}
	summary := lastResult(tailed.String())
	if code == -128 {
		code, summary = exitFailed, "Cancelled: administrator access was not granted."
	}
	if summary == "" {
		if code == exitOK {
			summary = "Done."
		} else {
			summary = fmt.Sprintf("Failed (exit code %d). Details: %s", code, logPath)
		}
	}
	macDialog(code == exitOK, summary)
	return code
}

// tailFile prints lines appended to path after offset until done is closed.
func tailFile(path string, offset int64, done <-chan struct{}, emit func(string)) {
	var partial string
	read := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil && fi.Size() < offset {
			offset = 0 // log was rotated or truncated
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return
		}
		b, _ := io.ReadAll(f)
		offset += int64(len(b))
		partial += string(b)
		for {
			i := strings.IndexByte(partial, '\n')
			if i < 0 {
				break
			}
			emit(partial[:i])
			partial = partial[i+1:]
		}
	}
	for {
		select {
		case <-done:
			read()
			if partial != "" {
				emit(partial)
			}
			return
		case <-time.After(400 * time.Millisecond):
			read()
		}
	}
}
