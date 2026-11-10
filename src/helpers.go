package main

// Pure helper functions shared by all platforms (kept OS-independent so they
// can be unit-tested on any build machine).

import (
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// shellQuote quotes s for POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleScriptString returns s as an AppleScript string literal.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

var osascriptCodeRe = regexp.MustCompile(`\((-?\d+)\)\s*$`)

// osascriptExitCode extracts the error number from osascript's stderr, e.g.
// "0:153: execution error: (3)" -> 3, "User canceled. (-128)" -> -128.
// For `do shell script`, the number is the shell command's exit status.
func osascriptExitCode(stderr string) int {
	m := osascriptCodeRe.FindStringSubmatch(strings.TrimSpace(stderr))
	if m == nil {
		return exitFailed
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return exitFailed
	}
	return n
}

// lastResult finds the summary on the last "RESULT: WORD | summary" log line.
func lastResult(log string) string {
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if _, after, ok := strings.Cut(lines[i], "RESULT: "); ok {
			if _, summary, ok := strings.Cut(after, " | "); ok {
				return strings.TrimSpace(summary)
			}
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// mapLinuxArch maps `uname -m` output to Tailscale's tarball architecture names.
func mapLinuxArch(uname string) string {
	u := strings.ToLower(strings.TrimSpace(uname))
	switch {
	case u == "x86_64" || u == "amd64":
		return "amd64"
	case u == "aarch64" || u == "arm64" || u == "armv8b" || u == "aarch64_be":
		return "arm64"
	case strings.HasPrefix(u, "arm"):
		return "arm"
	case u == "i386" || u == "i486" || u == "i586" || u == "i686" || u == "x86":
		return "386"
	case u == "riscv64":
		return "riscv64"
	}
	return u
}

// buildMSICommandLine builds a raw msiexec command line. msiexec parses its own
// command line and does not accept Go's default argument quoting for
// PROPERTY="value" pairs, so this is assembled by hand.
func buildMSICommandLine(msiexec, msi, logPath string, props map[string]string) string {
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	parts := []string{q(msiexec), "/i", q(msi), "/qn", "/norestart", "/L*v", q(logPath)}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+q(props[k]))
	}
	return strings.Join(parts, " ")
}

// sanitizeHostname turns s into a valid DNS label (lowercase, a-z0-9-, max 63).
func sanitizeHostname(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	return out
}

// parseSHA256 parses a ".sha256" sidecar file ("<hex>" or "<hex>  filename").
func parseSHA256(b []byte) (string, error) {
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return "", errors.New("empty checksum file")
	}
	h := strings.ToLower(f[0])
	if len(h) != 64 {
		return "", errors.New("malformed SHA-256 checksum")
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", errors.New("malformed SHA-256 checksum")
	}
	return h, nil
}

// extractJSON returns the outermost {...} object in s (the Tailscale CLI may
// print warnings before its JSON output).
func extractJSON(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return ""
	}
	return s[i : j+1]
}

// hasFlag reports whether args contains -name / --name / -name=x / --name=x.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		a = strings.TrimLeft(a, "-")
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// parseMajor returns the leading integer of a dotted version ("14.6.1" -> 14).
func parseMajor(v string) int {
	n, _ := strconv.Atoi(strings.SplitN(strings.TrimSpace(v), ".", 2)[0])
	return n
}

func resultWord(code int) string {
	switch code {
	case exitOK:
		return "SUCCESS"
	case exitUnsupported:
		return "UNSUPPORTED"
	case exitPending:
		return "PENDING"
	case exitConfigError:
		return "CONFIG_ERROR"
	}
	return "FAILED"
}
