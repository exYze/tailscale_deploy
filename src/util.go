package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---- logging ---------------------------------------------------------------

var (
	logMu   sync.Mutex
	logFile *os.File
	secrets []string
)

func initLog(path string) {
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot open log file %s: %v\n", path, err)
		return
	}
	logFile = f
}

func closeLog() {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}

func addSecret(s string) {
	if len(s) >= 8 {
		secrets = append(secrets, s)
	}
}

func redact(s string) string {
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, "[REDACTED]")
	}
	return s
}

// logf prints to the console and appends a timestamped line to the log file.
// Auth keys are always redacted.
func logf(format string, a ...any) {
	msg := redact(fmt.Sprintf(format, a...))
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Println(msg)
	if logFile != nil {
		ts := time.Now().Format("2006-01-02 15:04:05")
		for _, line := range strings.Split(msg, "\n") {
			fmt.Fprintf(logFile, "%s  %s\n", ts, line)
		}
	}
}

// ---- processes -------------------------------------------------------------

func runCmd(ctx context.Context, timeout time.Duration, quiet bool, name string, args ...string) (string, error) {
	return runCmdWith(ctx, timeout, quiet, nil, name, args...)
}

// runCmdWith runs a command, returning combined stdout+stderr. mutate may
// adjust the *exec.Cmd before it starts (environment, raw command line, ...).
func runCmdWith(ctx context.Context, timeout time.Duration, quiet bool, mutate func(*exec.Cmd), name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if mutate != nil {
		mutate(cmd)
	}
	if !quiet {
		logf("> %s %s", filepath.Base(name), strings.Join(args, " "))
	}
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if !quiet && s != "" {
		logf("  %s", strings.ReplaceAll(truncate(s, 6000), "\n", "\n  "))
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return s, fmt.Errorf("%s timed out after %s", filepath.Base(name), timeout)
	}
	if err != nil {
		return s, fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return s, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ---- downloads -------------------------------------------------------------

var httpClient = &http.Client{Timeout: 20 * time.Minute}

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tailscale-deploy/"+buildVersion)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return resp, nil
}

func withRetry(ctx context.Context, what string, fn func() error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt < 3 {
			d := time.Duration(3<<attempt) * time.Second
			logf("%s failed (%v); retrying in %s", what, err, d)
			if e := sleepCtx(ctx, d); e != nil {
				return e
			}
		}
	}
	return err
}

func fetchBytes(ctx context.Context, url string, max int64) ([]byte, error) {
	var body []byte
	err := withRetry(ctx, "fetching "+url, func() error {
		resp, err := httpGet(ctx, url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err = io.ReadAll(io.LimitReader(resp.Body, max))
		return err
	})
	return body, err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// downloadVerified downloads url to dest and verifies it against the
// url+".sha256" file published next to it. A cached file is reused only if its
// checksum still matches.
func downloadVerified(ctx context.Context, url, dest string) error {
	sumBytes, err := fetchBytes(ctx, url+".sha256", 4096)
	if err != nil {
		return fmt.Errorf("could not fetch the checksum %s.sha256: %w", url, err)
	}
	want, err := parseSHA256(sumBytes)
	if err != nil {
		return fmt.Errorf("%s.sha256: %w", url, err)
	}
	if got, err := fileSHA256(dest); err == nil && got == want {
		logf("Using already-downloaded %s (SHA-256 verified)", filepath.Base(dest))
		return nil
	}
	logf("Downloading %s", url)
	return withRetry(ctx, "download", func() error {
		tmp := dest + ".part"
		resp, err := httpGet(ctx, url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(tmp)
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			os.Remove(tmp)
			return fmt.Errorf("checksum mismatch for %s (got %s, expected %s)", filepath.Base(dest), got, want)
		}
		os.Remove(dest)
		if err := os.Rename(tmp, dest); err != nil {
			return err
		}
		logf("Downloaded %.1f MB, SHA-256 verified", float64(n)/1e6)
		return nil
	})
}
