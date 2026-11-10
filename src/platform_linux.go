//go:build linux

package main

// Linux: uses Tailscale's official install script (adds the distro package
// repository so devices keep getting updates through apt/dnf/zypper/pacman/apk).
// If that is not possible (unsupported distro, pinned version, internal
// mirror, offline repos), it installs Tailscale's static binaries and a
// systemd service instead.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const notReadyExitCode = exitFailed

func platformName() string { return "linux/" + runtime.GOARCH }

func tailscaleCLIPath() (string, bool) {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, true
	}
	for _, p := range []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale", "/usr/sbin/tailscale"} {
		if fileExists(p) {
			return p, true
		}
	}
	return "", false
}

func workDir() string {
	if isElevated() {
		return "/var/cache/tailscale-deploy"
	}
	return filepath.Join(os.TempDir(), "tailscale-deploy")
}

func logFilePath() string { return "/var/log/tailscale-deploy.log" }

func linuxArch() string {
	if out, err := exec.Command("uname", "-m").Output(); err == nil {
		if a := mapLinuxArch(string(out)); a != "" {
			return a
		}
	}
	return runtime.GOARCH
}

func osPrettyName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return "Linux"
}

var supportedArch = map[string]bool{"amd64": true, "arm64": true, "arm": true, "386": true, "riscv64": true}

func checkOSSupported() (string, error) {
	kernel := ""
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		kernel = strings.TrimSpace(string(out))
	}
	arch := linuxArch()
	desc := fmt.Sprintf("%s (%s, kernel %s)", osPrettyName(), arch, kernel)
	if !supportedArch[arch] {
		return desc, fmt.Errorf("%s: CPU architecture %q is not supported by this installer", desc, arch)
	}
	return desc, nil
}

func hasSystemd() bool { return fileExists("/run/systemd/system") }

func installTailscale(ctx context.Context, cfg *Config, dryRun bool) error {
	useScript := cfg.MirrorURL == "" && cfg.Version == "latest" &&
		(cfg.LinuxUseInstallScript == nil || *cfg.LinuxUseInstallScript)
	if useScript {
		err := runInstallScript(ctx, cfg, dryRun)
		if err == nil {
			if dryRun {
				return installFromTarball(ctx, cfg, true) // also verify the fallback path
			}
			if _, ok := tailscaleCLIPath(); ok {
				return nil
			}
			err = errors.New("tailscale was not found after the install script finished")
		}
		logf("The official install script did not work (%v); installing the static binaries instead.", err)
	}
	return installFromTarball(ctx, cfg, dryRun)
}

func runInstallScript(ctx context.Context, cfg *Config, dryRun bool) error {
	dir, _, err := prepareWorkDir()
	if err != nil {
		return err
	}
	b, err := fetchBytes(ctx, "https://tailscale.com/install.sh", 1<<20)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(b, []byte("#!")) {
		return errors.New("unexpected content downloaded from tailscale.com/install.sh")
	}
	script := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(script, b, 0o700); err != nil {
		return err
	}
	if dryRun {
		logf("Would run Tailscale's official installer: sh %s", script)
		return nil
	}
	logf("Running Tailscale's official installer (installs from your distribution's package manager)...")
	_, err = runCmdWith(ctx, 20*time.Minute, false, func(c *exec.Cmd) {
		c.Env = append(os.Environ(), "TRACK="+cfg.Track)
	}, "/bin/sh", script)
	return err
}

func installFromTarball(ctx context.Context, cfg *Config, dryRun bool) error {
	arch := linuxArch()
	ver, err := resolveVersion(ctx, cfg, func(ix *pkgIndex) string { return ix.TarballsVersion })
	if err != nil {
		return err
	}
	name := fmt.Sprintf("tailscale_%s_%s.tgz", ver, arch)
	dir, _, err := prepareWorkDir()
	if err != nil {
		return err
	}
	tgz := filepath.Join(dir, name)
	if err := downloadVerified(ctx, cfg.baseURL()+"/"+name, tgz); err != nil {
		return err
	}
	if dryRun {
		logf("Would install tailscale to /usr/local/bin, tailscaled to /usr/local/sbin, and register the tailscaled service.")
		return nil
	}
	files, err := extractTarball(tgz, "tailscale", "tailscaled", "tailscaled.service", "tailscaled.defaults")
	if err != nil {
		return err
	}
	if files["tailscale"] == nil || files["tailscaled"] == nil {
		return errors.New("the Tailscale archive did not contain the expected binaries")
	}
	logf("Installing Tailscale %s static binaries...", ver)
	if err := writeFileAtomic("/usr/local/bin/tailscale", files["tailscale"], 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic("/usr/local/sbin/tailscaled", files["tailscaled"], 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll("/var/lib/tailscale", 0o700); err != nil {
		return err
	}
	if hasSystemd() {
		unit := string(files["tailscaled.service"])
		if unit == "" {
			unit = defaultUnit
		}
		unit = strings.ReplaceAll(unit, "/usr/sbin/tailscaled", "/usr/local/sbin/tailscaled")
		if err := os.WriteFile("/etc/systemd/system/tailscaled.service", []byte(unit), 0o644); err != nil {
			return err
		}
		if !fileExists("/etc/default/tailscaled") {
			defaults := files["tailscaled.defaults"]
			if defaults == nil {
				defaults = []byte("PORT=\"41641\"\nFLAGS=\"\"\n")
			}
			os.MkdirAll("/etc/default", 0o755)
			if err := os.WriteFile("/etc/default/tailscaled", defaults, 0o644); err != nil {
				return err
			}
		}
		runCmd(ctx, time.Minute, false, "systemctl", "daemon-reload")
	}
	return nil
}

const defaultUnit = `[Unit]
Description=Tailscale node agent
Wants=network-pre.target
After=network-pre.target NetworkManager.service systemd-resolved.service

[Service]
EnvironmentFile=-/etc/default/tailscaled
ExecStart=/usr/sbin/tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/run/tailscale/tailscaled.sock --port=${PORT} $FLAGS
ExecStopPost=/usr/sbin/tailscaled --cleanup
Restart=on-failure
RuntimeDirectory=tailscale
RuntimeDirectoryMode=0755
StateDirectory=tailscale
StateDirectoryMode=0700
CacheDirectory=tailscale
CacheDirectoryMode=0750
Type=notify

[Install]
WantedBy=multi-user.target
`

func extractTarball(tgz string, want ...string) (map[string][]byte, error) {
	f, err := os.Open(tgz)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[w] = true
	}
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		base := path.Base(h.Name)
		if h.Typeflag != tar.TypeReg || !wanted[base] {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 256<<20))
		if err != nil {
			return nil, err
		}
		out[base] = b
	}
	return out, nil
}

func writeFileAtomic(dst string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tsdeploy-new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func ensureDaemonRunning(ctx context.Context) error {
	if !fileExists("/dev/net/tun") {
		runCmd(ctx, 30*time.Second, true, "modprobe", "tun")
	}
	if hasSystemd() {
		_, err := runCmd(ctx, 2*time.Minute, false, "systemctl", "enable", "--now", "tailscaled")
		return err
	}
	if _, err := exec.LookPath("rc-service"); err == nil { // OpenRC (Alpine, Gentoo)
		runCmd(ctx, time.Minute, false, "rc-update", "add", "tailscale", "default")
		_, err := runCmd(ctx, time.Minute, false, "rc-service", "tailscale", "start")
		return err
	}
	return startTailscaledDetached()
}

// startTailscaledDetached starts tailscaled directly on systems without
// systemd or OpenRC. This does not persist across reboots.
func startTailscaledDetached() error {
	for _, sock := range []string{"/run/tailscale/tailscaled.sock", "/var/run/tailscale/tailscaled.sock"} {
		if fileExists(sock) {
			return nil // already running
		}
	}
	bin := ""
	for _, p := range []string{"/usr/local/sbin/tailscaled", "/usr/sbin/tailscaled"} {
		if fileExists(p) {
			bin = p
			break
		}
	}
	if bin == "" {
		return errors.New("tailscaled binary not found")
	}
	os.MkdirAll("/var/run/tailscale", 0o755)
	logOut, _ := os.OpenFile("/var/log/tailscaled.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	cmd := exec.Command(bin, "--state=/var/lib/tailscale/tailscaled.state", "--socket=/var/run/tailscale/tailscaled.sock")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logOut, logOut
	if err := cmd.Start(); err != nil {
		return err
	}
	cmd.Process.Release()
	logf("WARNING: no systemd or OpenRC found. tailscaled was started now but will NOT start automatically after a reboot; add %s to this system's startup.", bin)
	return nil
}

func onDaemonSlow(ctx context.Context) {
	logf("Still waiting for tailscaled to start (check: systemctl status tailscaled)...")
}

func notReadyHint() string {
	return "tailscaled did not start. Check `systemctl status tailscaled` and `journalctl -u tailscaled`."
}

func notifyUser(ok bool, msg string) {} // the summary is already printed to the terminal

// relaunchElevated re-executes this program as root via pkexec (graphical
// password prompt when launched from a desktop) or sudo (terminal).
func relaunchElevated(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot find own executable:", err)
		return exitFailed
	}
	argv := append([]string{exe}, args...)
	desktop := (os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "") && !detectInteractive()
	try := func(tool string) {
		if p, err := exec.LookPath(tool); err == nil {
			syscall.Exec(p, append([]string{p}, argv...), os.Environ()) // only returns on failure
		}
	}
	if desktop {
		try("pkexec")
	}
	fmt.Println("Root access is required to install Tailscale.")
	try("sudo")
	try("pkexec")
	fmt.Fprintf(os.Stderr, "Run this as root, for example: sudo %s\n", exe)
	return exitFailed
}
