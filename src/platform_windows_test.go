//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// testSDDL is like workDirSDDL but owned by and granted to the current user,
// so the real Windows code paths can be exercised without elevation.
func testSDDL(t *testing.T) string {
	t.Helper()
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := u.User.Sid.String()
	if err != nil {
		t.Fatal(err)
	}
	return "O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")"
}

func assertSecured(t *testing.T, dir, sddl string) {
	t.Helper()
	sd, err := sddlToSD(sddl)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	want, _ := sdToSDDL(sd)
	got, err := dirSDDL(dir)
	if err != nil || !sddlMatches(got, want) {
		t.Fatalf("%s not secured: got %q (%v), want %q", dir, got, err, want)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("%s should be empty, has %d entries", dir, len(entries))
	}
}

func TestSecureDirCreatesAndReuses(t *testing.T) {
	sddl := testSDDL(t)
	dir := filepath.Join(t.TempDir(), "TailscaleDeploy")
	note, err := secureDir(dir, sddl)
	if err != nil || note != "" {
		t.Fatalf("create: note %q, err %v", note, err)
	}
	assertSecured(t, dir, sddl)
	if err := os.WriteFile(filepath.Join(dir, "deploy.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	note, err = secureDir(dir, sddl) // already secure: kept as is
	if err != nil || note != "" {
		t.Fatalf("reuse: note %q, err %v", note, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy.log")); err != nil {
		t.Fatal("the secured folder's contents should be kept")
	}
}

func TestSecureDirMovesAsideUnprotected(t *testing.T) {
	sddl := testSDDL(t)
	base := t.TempDir()
	cases := map[string]func(dir string){
		"inherited ACL (older version or another user)": func(dir string) {
			os.Mkdir(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "tailscale-setup.msi"), []byte("planted"), 0o644)
		},
		"file at the path": func(dir string) {
			os.WriteFile(dir, []byte("planted"), 0o644)
		},
		"junction": func(dir string) {
			target := dir + "-target"
			os.Mkdir(target, 0o755)
			os.WriteFile(filepath.Join(target, "keep.txt"), []byte("victim"), 0o644)
			if out, err := exec.Command("cmd", "/c", "mklink", "/J", dir, target).CombinedOutput(); err != nil {
				t.Fatalf("mklink: %v %s", err, out)
			}
		},
	}
	i := 0
	for name, setup := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(base, "TailscaleDeploy"+string(rune('0'+i)))
			setup(dir)
			note, err := secureDir(dir, sddl)
			if err != nil || !strings.Contains(note, "moved the unprotected folder") {
				t.Fatalf("note %q, err %v", note, err)
			}
			assertSecured(t, dir, sddl)
			if name == "junction" {
				if b, err := os.ReadFile(filepath.Join(dir+"-target", "keep.txt")); err != nil || string(b) != "victim" {
					t.Fatal("the junction's target must not be touched")
				}
			}
		})
	}
}

// A user who holds a file inside a pre-created folder open stops it from being
// moved aside; the deployment must then continue in a fresh, secured folder.
func TestSecureDirFallsBackWhenBlocked(t *testing.T) {
	sddl := testSDDL(t)
	dir := filepath.Join(t.TempDir(), "TailscaleDeploy")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := os.Create(filepath.Join(dir, "held-open.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := secureDir(dir, sddl); err == nil {
		t.Fatal("expected moving the folder aside to fail while a file in it is open")
	}
	got, note, err := secureDirOrFallback(dir, sddl)
	if err != nil || !strings.HasPrefix(got, dir+"-") || !strings.Contains(note, "using "+got+" instead") {
		t.Fatalf("got %q, note %q, err %v", got, note, err)
	}
	assertSecured(t, got, sddl)
	if _, err := os.Stat(filepath.Join(dir, "held-open.txt")); err != nil {
		t.Fatal("the blocked folder must be left alone")
	}
}

func TestWorkDirSDDLIsValid(t *testing.T) {
	sd, err := sddlToSD(workDirSDDL)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	got, err := sdToSDDL(sd)
	if err != nil || !sddlMatches(got, got) || !strings.HasPrefix(got, "O:BAD:P") {
		t.Fatalf("got %q, %v", got, err)
	}
}
