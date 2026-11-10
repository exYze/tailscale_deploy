package main

import (
	"bytes"
	"debug/macho"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCombine cross-compiles a tiny program for darwin/amd64 and darwin/arm64,
// merges them, and checks the universal binary contains both, byte-for-byte.
func TestCombine(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module hello\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"hi\") }\n"), 0o644)
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	var inputs []string
	for _, arch := range []string{"amd64", "arm64"} {
		out := filepath.Join(dir, "hello-"+arch)
		cmd := exec.Command(goBin, "build", "-o", out, ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH="+arch, "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", arch, err, b)
		}
		inputs = append(inputs, out)
	}
	fat := filepath.Join(dir, "hello-universal")
	if err := combine(fat, inputs); err != nil {
		t.Fatal(err)
	}
	ff, err := macho.OpenFat(fat)
	if err != nil {
		t.Fatalf("not a valid universal binary: %v", err)
	}
	defer ff.Close()
	if len(ff.Arches) != 2 {
		t.Fatalf("want 2 architectures, got %d", len(ff.Arches))
	}
	raw, _ := os.ReadFile(fat)
	want := map[macho.Cpu]string{macho.CpuAmd64: inputs[0], macho.CpuArm64: inputs[1]}
	for _, a := range ff.Arches {
		if a.Offset%(1<<alignLog2) != 0 {
			t.Errorf("%v slice offset %d is not 16 KiB aligned", a.Cpu, a.Offset)
		}
		orig, _ := os.ReadFile(want[a.Cpu])
		if !bytes.Equal(raw[a.Offset:a.Offset+a.Size], orig) {
			t.Errorf("%v slice differs from the original binary", a.Cpu)
		}
	}
	if err := combine(filepath.Join(dir, "dup"), []string{inputs[0], inputs[0]}); err == nil {
		t.Error("expected an error for duplicate architectures")
	}
}
