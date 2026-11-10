package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// pkgIndex is the subset of https://pkgs.tailscale.com/<track>/?mode=json we use.
type pkgIndex struct {
	Version         string
	MSIsVersion     string
	MacZipsVersion  string
	TarballsVersion string
}

// resolveVersion returns the pinned version, or asks pkgs.tailscale.com for
// the latest one on this platform.
func resolveVersion(ctx context.Context, cfg *Config, pick func(*pkgIndex) string) (string, error) {
	if cfg.Version != "latest" {
		return cfg.Version, nil
	}
	b, err := fetchBytes(ctx, cfg.baseURL()+"/?mode=json", 1<<20)
	if err != nil {
		return "", fmt.Errorf("could not look up the latest Tailscale version: %w", err)
	}
	var ix pkgIndex
	if err := json.Unmarshal(b, &ix); err != nil {
		return "", fmt.Errorf("could not parse the Tailscale package index: %w", err)
	}
	v := pick(&ix)
	if v == "" {
		v = ix.Version
	}
	if !versionRe.MatchString(v) {
		return "", fmt.Errorf("unexpected version %q in the Tailscale package index", v)
	}
	logf("Latest %s Tailscale version: %s", cfg.Track, v)
	return v, nil
}

// tsStatus is the subset of `tailscale status --json` we use.
type tsStatus struct {
	BackendState string
	AuthURL      string
	Self         *struct {
		HostName     string
		DNSName      string
		TailscaleIPs []string
	}
	CurrentTailnet *struct {
		Name string
	}
}

func (s *tsStatus) describe() string {
	name, ips, tailnet := "", "", ""
	if s.Self != nil {
		name = strings.TrimSuffix(s.Self.DNSName, ".")
		if name == "" {
			name = s.Self.HostName
		}
		ips = strings.Join(s.Self.TailscaleIPs, ", ")
	}
	if s.CurrentTailnet != nil {
		tailnet = s.CurrentTailnet.Name
	}
	d := name
	if ips != "" {
		d += " (" + ips + ")"
	}
	if tailnet != "" {
		d += " on tailnet " + tailnet
	}
	if d == "" {
		d = "state " + s.BackendState
	}
	return d
}

func getStatus(ctx context.Context, cli string) (*tsStatus, error) {
	out, err := runCLI(ctx, 20*time.Second, true, cli, "status", "--json")
	js := extractJSON(out)
	if js == "" {
		if err == nil {
			err = errors.New("no status returned")
		}
		return nil, fmt.Errorf("%v: %s", err, truncate(out, 300))
	}
	var st tsStatus
	if e := json.Unmarshal([]byte(js), &st); e != nil || st.BackendState == "" {
		return nil, fmt.Errorf("unexpected status output: %s", truncate(out, 300))
	}
	return &st, nil
}

// waitForDaemon waits until the Tailscale service answers on its local API.
// onSlow runs once if this takes longer than ~25 seconds.
func waitForDaemon(ctx context.Context, cli string, timeout time.Duration, onSlow func(context.Context)) (*tsStatus, error) {
	start := time.Now()
	var firstAnswer time.Time
	var lastErr error
	slowFired := false
	for {
		st, err := getStatus(ctx, cli)
		if err == nil {
			if firstAnswer.IsZero() {
				firstAnswer = time.Now()
			}
			// "NoState" means the daemon is still initialising; give it a moment.
			if st.BackendState != "NoState" || time.Since(firstAnswer) > 15*time.Second {
				return st, nil
			}
		} else {
			lastErr = err
		}
		if !slowFired && onSlow != nil && time.Since(start) > 25*time.Second {
			slowFired = true
			onSlow(ctx)
		}
		if time.Since(start) > timeout {
			return nil, fmt.Errorf("Tailscale did not become ready within %s (last error: %v)", timeout, lastErr)
		}
		if err := sleepCtx(ctx, 3*time.Second); err != nil {
			return nil, err
		}
	}
}

// waitForState polls until the backend is Running or waiting for admin
// approval, or until timeout. It returns the last status seen.
func waitForState(ctx context.Context, cli string, timeout time.Duration) *tsStatus {
	deadline := time.Now().Add(timeout)
	var last *tsStatus
	for {
		if st, err := getStatus(ctx, cli); err == nil {
			last = st
			if st.BackendState == "Running" || st.BackendState == "NeedsMachineAuth" {
				return st
			}
		}
		if time.Now().After(deadline) || sleepCtx(ctx, 3*time.Second) != nil {
			return last
		}
	}
}

func deviceHostname(prefix string) string {
	if prefix == "" {
		return ""
	}
	h, _ := os.Hostname()
	h = strings.SplitN(h, ".", 2)[0]
	return sanitizeHostname(prefix + h)
}

// buildUpArgs builds the `tailscale up` arguments. The key is passed as
// "file:<path>" so it never appears in process listings or RMM/EDR command-line logs.
func buildUpArgs(cfg *Config, keyPath, hostname string) []string {
	args := []string{
		"up",
		"--auth-key=file:" + keyPath,
		"--reset",
		fmt.Sprintf("--timeout=%ds", cfg.ConnectTimeoutSeconds),
	}
	if len(cfg.Tags) > 0 {
		args = append(args, "--advertise-tags="+strings.Join(cfg.Tags, ","))
	}
	if hostname != "" {
		args = append(args, "--hostname="+hostname)
	}
	if cfg.LoginServer != "" {
		args = append(args, "--login-server="+cfg.LoginServer)
	}
	if cfg.AcceptRoutes {
		args = append(args, "--accept-routes")
	}
	if cfg.AcceptDNS != nil {
		args = append(args, fmt.Sprintf("--accept-dns=%t", *cfg.AcceptDNS))
	}
	if cfg.ForceReauth {
		args = append(args, "--force-reauth")
	}
	args = append(args, platformUpArgs()...)
	args = append(args, cfg.ExtraUpArgs...)
	return args
}

func writeKeyFile(key string) (string, func(), error) {
	f, err := os.CreateTemp("", "tsdeploy-*.key") // created with owner-only permissions
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	var once sync.Once
	cleanup := func() { once.Do(func() { os.Remove(path) }) }
	if _, err := f.WriteString(key); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

func upErrorHint(out string) string {
	o := strings.ToLower(out)
	switch {
	case strings.Contains(o, "invalid key") || strings.Contains(o, "expired") || strings.Contains(o, "not valid") || strings.Contains(o, "unauthorized"):
		return " -> The auth key is invalid, expired or revoked. Create a new one (or an OAuth client secret) and rebuild."
	case strings.Contains(o, "tag") && (strings.Contains(o, "invalid") || strings.Contains(o, "not permitted") || strings.Contains(o, "not allowed")):
		return " -> A tag was rejected. Add it to \"tagOwners\" in your tailnet policy file, and to the OAuth client's tags."
	case strings.Contains(o, "requires mentioning all non-default flags"):
		return " -> Run again with --force-reauth."
	}
	return ""
}

// deploy installs Tailscale if needed and connects the device. It returns an
// exit code and a one-line human summary.
func deploy(ctx context.Context, cfg *Config, opts options) (int, string) {
	cli, installed := tailscaleCLIPath()

	if opts.dryRun {
		if installed {
			logf("Tailscale is already installed at %s (a real run would skip installing it).", cli)
		}
		if err := installTailscale(ctx, cfg, true); err != nil {
			return exitFailed, "Dry run failed: " + err.Error()
		}
		logf("Would run: tailscale %s", strings.Join(buildUpArgs(cfg, "<temporary key file>", deviceHostname(cfg.HostnamePrefix)), " "))
		return exitOK, "Dry run passed: the installer was downloaded and verified. Nothing was changed."
	}

	if installed && !cfg.ForceReauth {
		if st, err := getStatus(ctx, cli); err == nil && st.BackendState == "Running" {
			return exitOK, "Already connected: " + st.describe()
		}
	}

	if !installed {
		if err := installTailscale(ctx, cfg, false); err != nil {
			return exitFailed, "Installing Tailscale failed: " + err.Error()
		}
		var ok bool
		if cli, ok = tailscaleCLIPath(); !ok {
			return exitFailed, "Tailscale was installed but its command-line tool could not be found."
		}
		logf("Tailscale installed.")
	} else {
		logf("Tailscale is already installed (%s).", cli)
	}

	if err := ensureDaemonRunning(ctx); err != nil {
		logf("Note: %v", err)
	}
	logf("Waiting for the Tailscale service...")
	timeout := time.Duration(cfg.ConnectTimeoutSeconds) * time.Second
	if _, err := waitForDaemon(ctx, cli, timeout, onDaemonSlow); err != nil {
		logf("%v", err)
		return notReadyExitCode, notReadyHint()
	}
	return connect(ctx, cfg, cli, timeout)
}

func connect(ctx context.Context, cfg *Config, cli string, timeout time.Duration) (int, string) {
	keyPath, cleanup, err := writeKeyFile(cfg.effectiveAuthKey())
	if err != nil {
		return exitFailed, "Could not write the temporary key file: " + err.Error()
	}
	defer cleanup()

	logf("Connecting to your tailnet...")
	args := buildUpArgs(cfg, keyPath, deviceHostname(cfg.HostnamePrefix))
	out, upErr := runCLI(ctx, timeout+time.Minute, false, cli, args...)
	cleanup() // the key is no longer needed on disk

	st := waitForState(ctx, cli, 45*time.Second)
	if st != nil {
		switch st.BackendState {
		case "Running":
			return exitOK, "Connected: " + st.describe()
		case "NeedsMachineAuth":
			return exitPending, "Enrolled, but the device is waiting for approval in the Tailscale admin console (Machines page). Use a pre-approved key to skip this step."
		}
	}
	if upErr != nil {
		return exitFailed, "Connecting failed: " + upErr.Error() + upErrorHint(out)
	}
	state := "unknown"
	if st != nil {
		state = st.BackendState
	}
	return exitFailed, "Tailscale did not finish connecting (state: " + state + ")."
}
