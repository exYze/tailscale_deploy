//go:build windows

package main

// The Windows build is compiled as a 32-bit (386) program so ONE .exe runs on
// every Windows 10/11 and Server 2016+ machine: x64 (via WoW64) and ARM64
// (via x86 emulation). It detects the real CPU and installs the native MSI.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
	modShell32  = syscall.NewLazyDLL("shell32.dll")
	modUser32   = syscall.NewLazyDLL("user32.dll")
	modNtdll    = syscall.NewLazyDLL("ntdll.dll")

	procIsWow64Process2       = modKernel32.NewProc("IsWow64Process2")
	procGetConsoleProcessList = modKernel32.NewProc("GetConsoleProcessList")
	procProcessIdToSessionId  = modKernel32.NewProc("ProcessIdToSessionId")
	procShellExecuteW         = modShell32.NewProc("ShellExecuteW")
	procMessageBoxW           = modUser32.NewProc("MessageBoxW")
	procRtlGetVersion         = modNtdll.NewProc("RtlGetVersion")
)

const notReadyExitCode = exitFailed

func platformName() string { return "windows/" + runtime.GOARCH }

func systemRoot() string {
	if s := os.Getenv("SystemRoot"); s != "" {
		return s
	}
	return `C:\Windows`
}

func programData() string {
	if s := os.Getenv("ProgramData"); s != "" {
		return s
	}
	return `C:\ProgramData`
}

// programFiles returns the 64-bit Program Files folder even from this 32-bit
// process (where %ProgramFiles% points at "Program Files (x86)").
func programFiles() string {
	if p := os.Getenv("ProgramW6432"); p != "" {
		return p
	}
	if p := os.Getenv("ProgramFiles"); p != "" {
		return p
	}
	return `C:\Program Files`
}

func tailscaleCLIPath() (string, bool) {
	p := filepath.Join(programFiles(), "Tailscale", "tailscale.exe")
	return p, fileExists(p)
}

func workDir() string {
	if isElevated() {
		return filepath.Join(programData(), "TailscaleDeploy")
	}
	return filepath.Join(os.TempDir(), "TailscaleDeploy")
}

func logFilePath() string { return filepath.Join(programData(), "TailscaleDeploy", "deploy.log") }

type osVersionInfoEx struct {
	OSVersionInfoSize uint32
	MajorVersion      uint32
	MinorVersion      uint32
	BuildNumber       uint32
	PlatformID        uint32
	CSDVersion        [128]uint16
	ServicePackMajor  uint16
	ServicePackMinor  uint16
	SuiteMask         uint16
	ProductType       byte
	Reserved          byte
}

func windowsVersion() (info osVersionInfoEx) {
	info.OSVersionInfoSize = uint32(unsafe.Sizeof(info))
	procRtlGetVersion.Call(uintptr(unsafe.Pointer(&info)))
	return info
}

func checkOSSupported() (string, error) {
	v := windowsVersion()
	name := "Windows"
	switch {
	case v.ProductType != 1: // not VER_NT_WORKSTATION
		name = "Windows Server"
	case v.MajorVersion == 10 && v.BuildNumber >= 22000:
		name = "Windows 11"
	case v.MajorVersion == 10:
		name = "Windows 10"
	}
	desc := fmt.Sprintf("%s (%d.%d build %d, %s)", name, v.MajorVersion, v.MinorVersion, v.BuildNumber, nativeArch())
	if v.MajorVersion < 10 {
		return desc, fmt.Errorf("%s is not supported: Tailscale requires Windows 10 / Windows Server 2016 or newer", desc)
	}
	return desc, nil
}

// nativeArch returns the MSI architecture name for the machine's real CPU.
func nativeArch() string {
	if procIsWow64Process2.Find() == nil {
		var processMachine, nativeMachine uint16
		h, _ := syscall.GetCurrentProcess()
		r, _, _ := procIsWow64Process2.Call(uintptr(h), uintptr(unsafe.Pointer(&processMachine)), uintptr(unsafe.Pointer(&nativeMachine)))
		if r != 0 {
			switch nativeMachine {
			case 0x8664:
				return "amd64"
			case 0xAA64:
				return "arm64"
			case 0x014c:
				return "x86"
			}
		}
	}
	a := os.Getenv("PROCESSOR_ARCHITEW6432")
	if a == "" {
		a = os.Getenv("PROCESSOR_ARCHITECTURE")
	}
	switch strings.ToUpper(a) {
	case "AMD64":
		return "amd64"
	case "ARM64":
		return "arm64"
	case "X86":
		return "x86"
	}
	if runtime.GOARCH == "386" {
		return "x86"
	}
	return runtime.GOARCH
}

func isElevated() bool {
	p, err := syscall.GetCurrentProcess()
	if err != nil {
		return false
	}
	var token syscall.Token
	if err := syscall.OpenProcessToken(p, syscall.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	var elevation, n uint32
	const tokenElevation = 20
	if err := syscall.GetTokenInformation(token, tokenElevation, (*byte)(unsafe.Pointer(&elevation)), 4, &n); err != nil {
		return false
	}
	return elevation != 0
}

// canShowUI is false in session 0 (services, SYSTEM via RMM), where a message
// box would be invisible and block forever.
func canShowUI() bool {
	var sid uint32
	r, _, _ := procProcessIdToSessionId.Call(uintptr(os.Getpid()), uintptr(unsafe.Pointer(&sid)))
	return r == 0 || sid != 0
}

// detectInteractive reports whether this process owns its console window,
// i.e. it was started by double-clicking rather than from a shell or script.
func detectInteractive() bool {
	var pids [8]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

func notifyUser(ok bool, msg string) {
	const (
		mbIconError       = 0x10
		mbIconInformation = 0x40
		mbSetForeground   = 0x10000
		mbTopmost         = 0x40000
	)
	flags := uintptr(mbSetForeground | mbTopmost)
	if ok {
		flags |= mbIconInformation
	} else {
		flags |= mbIconError
	}
	text, _ := syscall.UTF16PtrFromString(msg + "\n\nLog: " + logFilePath())
	title, _ := syscall.UTF16PtrFromString("Tailscale Deploy")
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), flags)
}

// relaunchElevated re-runs this program through the UAC "Run as administrator"
// prompt. The elevated copy opens its own window and reports the result.
func relaunchElevated(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot find own executable:", err)
		return exitFailed
	}
	params := make([]string, 0, len(args)+1)
	for _, a := range args {
		params = append(params, syscall.EscapeArg(a))
	}
	if !hasFlag(args, "interactive") {
		params = append(params, "--interactive")
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	par, _ := syscall.UTF16PtrFromString(strings.Join(params, " "))
	const swShowNormal = 1
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(par)), 0, swShowNormal)
	if r <= 32 {
		msg := "Administrator rights are required to install Tailscale."
		if r == 5 { // SE_ERR_ACCESSDENIED: the user clicked "No" on the UAC prompt
			msg += " The administrator prompt was declined."
		} else {
			msg += fmt.Sprintf(" (ShellExecute error %d)", r)
		}
		msg += "\n\nRight-click the file and choose \"Run as administrator\", or deploy it with --silent from an Administrator/SYSTEM context."
		fmt.Fprintln(os.Stderr, msg)
		if canShowUI() {
			notifyUser(false, msg)
		}
		return exitFailed
	}
	fmt.Println("Continuing in the administrator window...")
	return exitOK
}

func msiexecPath() string { return filepath.Join(systemRoot(), "System32", "msiexec.exe") }

func installTailscale(ctx context.Context, cfg *Config, dryRun bool) error {
	arch := nativeArch()
	ver, err := resolveVersion(ctx, cfg, func(ix *pkgIndex) string { return ix.MSIsVersion })
	if err != nil {
		return err
	}
	name := fmt.Sprintf("tailscale-setup-%s-%s.msi", ver, arch)
	dir := workDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	msi := filepath.Join(dir, name)
	if err := downloadVerified(ctx, cfg.baseURL()+"/"+name, msi); err != nil {
		return err
	}
	if err := verifyAuthenticode(ctx, msi); err != nil {
		return err
	}
	msiLog := filepath.Join(dir, "msiexec.log")
	cmdline := buildMSICommandLine(msiexecPath(), msi, msiLog, cfg.WindowsMSIProperties)
	if dryRun {
		logf("Would run: %s", cmdline)
		return nil
	}
	logf("Installing Tailscale %s (%s) silently...", ver, arch)
	logf("> %s", cmdline)
	for attempt := 1; ; attempt++ {
		_, err := runCmdWith(ctx, 20*time.Minute, true, func(c *exec.Cmd) {
			c.SysProcAttr = &syscall.SysProcAttr{CmdLine: cmdline}
		}, msiexecPath())
		switch code := exitCode(err); code {
		case 0:
			return nil
		case 3010, 1641:
			logf("Installed. (Windows reports a reboot is pending; Tailscale works without it.)")
			return nil
		case 1618: // another installation is already in progress
			if attempt < 6 {
				logf("Another Windows installation is in progress; waiting 30s (attempt %d of 6)...", attempt)
				if err := sleepCtx(ctx, 30*time.Second); err != nil {
					return err
				}
				continue
			}
			fallthrough
		default:
			return fmt.Errorf("msiexec exited with code %d (%v); details in %s", code, err, msiLog)
		}
	}
}

// verifyAuthenticode checks the MSI is validly signed by Tailscale. If the
// check itself cannot run (e.g. PowerShell blocked), the SHA-256 match from
// pkgs.tailscale.com stands.
func verifyAuthenticode(ctx context.Context, path string) error {
	ps := filepath.Join(systemRoot(), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	script := "$s = Get-AuthenticodeSignature -LiteralPath '" + strings.ReplaceAll(path, "'", "''") + "'; " +
		"Write-Output ('SIG|' + $s.Status + '|' + $s.SignerCertificate.Subject)"
	out, err := runCmd(ctx, 2*time.Minute, true, ps, "-NoProfile", "-NonInteractive", "-Command", script)
	idx := strings.LastIndex(out, "SIG|")
	if err != nil || idx < 0 {
		logf("Note: could not check the installer's digital signature (%v); relying on the SHA-256 checksum.", err)
		return nil
	}
	parts := strings.SplitN(strings.TrimSpace(out[idx:]), "|", 3)
	status, subject := "", ""
	if len(parts) == 3 {
		status, subject = parts[1], parts[2]
	}
	if status != "Valid" || !strings.Contains(strings.ToLower(subject), "tailscale") {
		return fmt.Errorf("the installer's digital signature is not valid (status %q, signer %q)", status, subject)
	}
	cn := subject
	if i := strings.Index(cn, "CN="); i >= 0 {
		cn = strings.SplitN(cn[i+3:], ",", 2)[0]
	}
	logf("Installer signature OK (signed by %s)", strings.Trim(cn, `"`))
	return nil
}

func ensureDaemonRunning(ctx context.Context) error {
	runCmd(ctx, 30*time.Second, true, filepath.Join(systemRoot(), "System32", "sc.exe"), "start", "Tailscale")
	return nil
}

// --unattended keeps Tailscale connected when no user is logged in and after reboots.
func platformUpArgs() []string { return []string{"--unattended"} }

func onDaemonSlow(ctx context.Context) {
	logf("Still waiting for the Tailscale service to start...")
}

func notReadyHint() string {
	return "Tailscale was installed but its Windows service did not respond. Check the \"Tailscale\" service, and " +
		filepath.Join(workDir(), "msiexec.log") + "."
}
