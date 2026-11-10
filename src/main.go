// Command tailscale-deploy installs Tailscale with administrator rights and
// connects the device to your tailnet using an embedded auth key or OAuth
// client secret.
//
// Double-click it to run interactively (it asks for admin rights once), or run
// it with --silent from an RMM/MDM/GPO/SSH session that is already
// Administrator/SYSTEM/root.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"
)

var buildVersion = "dev"

const (
	exitOK          = 0
	exitFailed      = 1
	exitUnsupported = 2
	exitPending     = 3 // installed, but waiting for a user/admin approval
	exitConfigError = 4
)

type options struct {
	silent         bool
	interactive    bool
	dryRun         bool
	checkConfig    bool
	showVersion    bool
	authKey        string
	tags           string
	hostnamePrefix string
	loginServer    string
	forceReauth    bool
}

func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("tailscale-deploy", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Tailscale Deploy %s - installs Tailscale and connects this device to your tailnet.\n\nUsage: %s [flags]\n\n", buildVersion, os.Args[0])
		fs.PrintDefaults()
		fmt.Fprintf(fs.Output(), "\nExit codes: 0 success/already connected, 1 failed, 2 unsupported OS, 3 pending approval, 4 configuration error.\n")
	}
	fs.BoolVar(&o.silent, "silent", false, "never show dialogs or prompts (for RMM/MDM/GPO/scripts; must already run as Administrator/SYSTEM/root)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "check this device, download and verify the installer, but change nothing")
	fs.BoolVar(&o.checkConfig, "check-config", false, "validate the embedded configuration and exit")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	fs.StringVar(&o.authKey, "authkey", "", "auth key or OAuth client secret (overrides the embedded one; the TS_DEPLOY_AUTHKEY environment variable also works)")
	fs.StringVar(&o.authKey, "auth-key", "", "alias for --authkey")
	fs.StringVar(&o.tags, "tags", "", "comma-separated tags to advertise, e.g. tag:endpoint (overrides the embedded tags)")
	fs.StringVar(&o.hostnamePrefix, "hostname-prefix", "", "prefix for this device's name in Tailscale")
	fs.StringVar(&o.loginServer, "login-server", "", "custom coordination server URL (for example Headscale)")
	fs.BoolVar(&o.forceReauth, "force-reauth", false, "re-enroll even if this device is already connected to a tailnet")
	fs.BoolVar(&o.interactive, "interactive", false, "internal: set when relaunched with administrator rights")
	err := fs.Parse(args)
	return o, err
}

func main() { os.Exit(run()) }

func run() int {
	opts, err := parseFlags(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		return exitConfigError
	}
	if opts.showVersion {
		fmt.Printf("tailscale-deploy %s (%s)\n", buildVersion, platformName())
		return exitOK
	}
	interactive := !opts.silent && canShowUI() && (opts.interactive || detectInteractive())
	fail := func(code int, msg string) int {
		fmt.Fprintln(os.Stderr, msg)
		if interactive {
			notifyUser(false, msg)
		}
		return code
	}

	cfg, err := loadConfig(embeddedConfigJSON)
	if err != nil {
		return fail(exitConfigError, "The embedded configuration is invalid: "+err.Error())
	}
	cfg.applyOverrides(opts, os.Getenv("TS_DEPLOY_AUTHKEY"))
	if err := cfg.normalize(); err != nil {
		return fail(exitConfigError, "Configuration error: "+err.Error())
	}
	for _, s := range cfg.secrets() {
		addSecret(s)
	}
	if opts.checkConfig {
		fmt.Println("Configuration OK:", cfg.summary())
		return exitOK
	}

	osDesc, err := checkOSSupported()
	if err != nil {
		fmt.Fprintln(os.Stderr, "RESULT: UNSUPPORTED | "+err.Error())
		if interactive {
			notifyUser(false, err.Error())
		}
		return exitUnsupported
	}

	if !isElevated() && !opts.dryRun {
		if opts.silent {
			return fail(exitFailed, "--silent requires running as Administrator/SYSTEM (Windows) or root (macOS/Linux).")
		}
		return relaunchElevated(os.Args[1:])
	}

	var workDirNote string
	if isElevated() {
		// On Windows the log lives in the working folder, which must be
		// secured before anything is written to it.
		_, note, err := prepareWorkDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: not writing a log file: "+err.Error())
		} else {
			initLog(logFilePath())
		}
		workDirNote = note
	}
	logf("=== Tailscale Deploy %s | %s | %s ===", buildVersion, osDesc, time.Now().Format(time.RFC3339))
	if workDirNote != "" {
		logf("Note: %s", workDirNote)
	}
	logf("Config: %s", cfg.summary())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, summary := deploy(ctx, cfg, opts)
	logf("RESULT: %s | %s", resultWord(code), summary)
	closeLog()
	if interactive {
		notifyUser(code == exitOK, summary)
	}
	return code
}
