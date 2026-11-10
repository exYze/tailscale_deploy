package main

import (
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`/Users/a b/it's`); got != `'/Users/a b/it'\''s'` {
		t.Fatalf("got %s", got)
	}
}

func TestAppleScriptString(t *testing.T) {
	if got := appleScriptString(`say "hi" \ bye`); got != `"say \"hi\" \\ bye"` {
		t.Fatalf("got %s", got)
	}
}

func TestOsascriptExitCode(t *testing.T) {
	cases := map[string]int{
		"0:153: execution error:  (3)":                  3,
		"0:200: execution error: User canceled. (-128)\n": -128,
		"something unexpected":                          exitFailed,
	}
	for in, want := range cases {
		if got := osascriptExitCode(in); got != want {
			t.Errorf("osascriptExitCode(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLastResult(t *testing.T) {
	log := "2026-01-01 10:00:00  RESULT: FAILED | old\n2026-01-01 10:01:00  Connecting\n2026-01-01 10:02:00  RESULT: SUCCESS | Connected: mac (100.64.0.1)\n"
	if got := lastResult(log); got != "Connected: mac (100.64.0.1)" {
		t.Fatalf("got %q", got)
	}
	if got := lastResult("nothing here"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestMapLinuxArch(t *testing.T) {
	cases := map[string]string{"x86_64\n": "amd64", "aarch64": "arm64", "armv7l": "arm", "armv6l": "arm", "i686": "386", "riscv64": "riscv64", "mips": "mips"}
	for in, want := range cases {
		if got := mapLinuxArch(in); got != want {
			t.Errorf("mapLinuxArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildMSICommandLine(t *testing.T) {
	got := buildMSICommandLine(`C:\Windows\System32\msiexec.exe`, `C:\ProgramData\TailscaleDeploy\ts.msi`, `C:\ProgramData\TailscaleDeploy\msi.log`,
		map[string]string{"TS_UNATTENDEDMODE": "always", "TS_ADMINCONSOLE": `say "hi"`})
	want := `"C:\Windows\System32\msiexec.exe" /i "C:\ProgramData\TailscaleDeploy\ts.msi" /qn /norestart /L*v "C:\ProgramData\TailscaleDeploy\msi.log" TS_ADMINCONSOLE="say ""hi""" TS_UNATTENDEDMODE="always"`
	if got != want {
		t.Fatalf("\ngot  %s\nwant %s", got, want)
	}
}

func TestSanitizeHostname(t *testing.T) {
	cases := map[string]string{"Corp-DESKTOP_01": "corp-desktop-01", "--a..b--": "a-b", strings.Repeat("x", 70): strings.Repeat("x", 63)}
	for in, want := range cases {
		if got := sanitizeHostname(in); got != want {
			t.Errorf("sanitizeHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSHA256(t *testing.T) {
	h := "3c41318c83c5d95d0b017b1baa08ba7c99fd5334166122bbb1931e55af99839c"
	if got, err := parseSHA256([]byte(strings.ToUpper(h) + "  file.msi\n")); err != nil || got != h {
		t.Fatalf("got %q %v", got, err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("z", 64)} {
		if _, err := parseSHA256([]byte(bad)); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	if got := extractJSON("Warning: version mismatch\n{\"BackendState\":\"Running\",\"Self\":{}}\n"); got != `{"BackendState":"Running","Self":{}}` {
		t.Fatalf("got %q", got)
	}
	if extractJSON("failed to connect") != "" {
		t.Fatal("expected empty")
	}
}

func TestHasFlag(t *testing.T) {
	args := []string{"--silent", "-tags=tag:a"}
	if !hasFlag(args, "silent") || !hasFlag(args, "tags") || hasFlag(args, "interactive") {
		t.Fatal("hasFlag mismatch")
	}
}

func TestDescribe(t *testing.T) {
	st := &tsStatus{BackendState: "Running"}
	if st.describe() != "state Running" {
		t.Fatalf("got %q", st.describe())
	}
}
