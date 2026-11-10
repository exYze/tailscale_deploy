package main

import (
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestLoadConfig(t *testing.T) {
	c, err := loadConfig([]byte("\xef\xbb\xbf  "))
	if err != nil || c.AuthKey != "" {
		t.Fatalf("empty/BOM config: %v %+v", err, c)
	}
	c, err = loadConfig([]byte("\xef\xbb\xbf{\"authKey\":\"tskey-auth-abc\",\"tags\":\"tag:a, tag:b\"}"))
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthKey != "tskey-auth-abc" || len(c.Tags) != 2 || c.Tags[1] != "tag:b" {
		t.Fatalf("got %+v", c)
	}
	c, err = loadConfig([]byte(`{"tags":["tag:a","tag:b,tag:c"]}`))
	if err != nil || len(c.Tags) != 3 {
		t.Fatalf("array tags: %v %v", err, c.Tags)
	}
	if _, err := loadConfig([]byte(`{"tag":["tag:a"]}`)); err == nil {
		t.Fatal("expected unknown field (typo) to be rejected")
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"missing key", Config{}, "no auth key"},
		{"bad prefix", Config{AuthKey: "abc123456"}, `must start with "tskey-"`},
		{"headscale key ok with login server", Config{AuthKey: "abcdef123", LoginServer: "https://hs.example.com"}, ""},
		{"bad tag", Config{AuthKey: "tskey-auth-x", Tags: stringList{"endpoint"}}, "invalid tag"},
		{"oauth needs tags", Config{AuthKey: "tskey-client-x"}, "needs at least one tag"},
		{"oauth ok", Config{AuthKey: "tskey-client-x", Tags: stringList{"tag:endpoint"}}, ""},
		{"bad version", Config{AuthKey: "tskey-auth-x", Version: "1.2"}, "invalid version"},
		{"v-prefixed version ok", Config{AuthKey: "tskey-auth-x", Version: "v1.104.5"}, ""},
		{"bad track", Config{AuthKey: "tskey-auth-x", Track: "beta"}, "invalid track"},
		{"mirror needs pinned version", Config{AuthKey: "tskey-auth-x", MirrorURL: "https://m.example.com/ts"}, "pinned"},
		{"mirror ok", Config{AuthKey: "tskey-auth-x", MirrorURL: "https://m.example.com/ts/", Version: "1.104.5"}, ""},
		{"bad msi prop", Config{AuthKey: "tskey-auth-x", WindowsMSIProperties: map[string]string{"ts x": "1"}}, "invalid MSI property"},
		{"key in extra args", Config{AuthKey: "tskey-auth-x", ExtraUpArgs: []string{"--authkey=foo"}}, "not in extraUpArgs"},
		{"extra arg not a flag", Config{AuthKey: "tskey-auth-x", ExtraUpArgs: []string{"ssh"}}, "must be a flag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.cfg
			err := c.normalize()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got error %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeDefaults(t *testing.T) {
	c := Config{AuthKey: " tskey-auth-x ", MirrorURL: "https://m.example.com/ts/", Version: "1.104.5"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.AuthKey != "tskey-auth-x" || c.Track != "stable" || c.ConnectTimeoutSeconds != 180 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.WindowsMSIProperties["TS_UNATTENDEDMODE"] != "always" {
		t.Fatal("TS_UNATTENDEDMODE should default to always")
	}
	if c.baseURL() != "https://m.example.com/ts" {
		t.Fatalf("baseURL = %q", c.baseURL())
	}
	c2 := Config{AuthKey: "tskey-auth-x", Track: "Unstable"}
	c2.normalize()
	if c2.baseURL() != "https://pkgs.tailscale.com/unstable" || c2.Version != "latest" {
		t.Fatalf("got %q %q", c2.baseURL(), c2.Version)
	}
	c3 := Config{AuthKey: "tskey-auth-x", WindowsMSIProperties: map[string]string{"TS_UNATTENDEDMODE": "never"}}
	c3.normalize()
	if c3.WindowsMSIProperties["TS_UNATTENDEDMODE"] != "never" {
		t.Fatal("an explicit TS_UNATTENDEDMODE must be kept")
	}
}

func TestEffectiveAuthKey(t *testing.T) {
	c := Config{AuthKey: "tskey-client-abc"}
	if got := c.effectiveAuthKey(); got != "tskey-client-abc?ephemeral=false&preauthorized=true" {
		t.Fatalf("oauth default: %q", got)
	}
	c.OAuthPreauthorized = boolPtr(false)
	if got := c.effectiveAuthKey(); got != "tskey-client-abc?ephemeral=false&preauthorized=false" {
		t.Fatalf("oauth not preauthorized: %q", got)
	}
	c = Config{AuthKey: "tskey-client-abc?ephemeral=true"}
	if got := c.effectiveAuthKey(); got != "tskey-client-abc?ephemeral=true" {
		t.Fatalf("explicit params must be kept: %q", got)
	}
	c = Config{AuthKey: "tskey-auth-abc"}
	if got := c.effectiveAuthKey(); got != "tskey-auth-abc" {
		t.Fatalf("auth key must be unchanged: %q", got)
	}
}

func TestOverrides(t *testing.T) {
	c := Config{AuthKey: "tskey-auth-embedded", Tags: stringList{"tag:a"}}
	c.applyOverrides(options{tags: "tag:x,tag:y", forceReauth: true}, "tskey-auth-env")
	if c.AuthKey != "tskey-auth-env" || len(c.Tags) != 2 || !c.ForceReauth {
		t.Fatalf("env override: %+v", c)
	}
	c.applyOverrides(options{authKey: "tskey-auth-flag"}, "tskey-auth-env")
	if c.AuthKey != "tskey-auth-flag" {
		t.Fatalf("flag must win over env: %q", c.AuthKey)
	}
}

func TestKeyNeverInArgsOrLogs(t *testing.T) {
	secrets = nil
	c := Config{AuthKey: "tskey-client-SUPERSECRET123", Tags: stringList{"tag:endpoint"}, HostnamePrefix: "corp-"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	for _, s := range c.secrets() {
		addSecret(s)
	}
	args := strings.Join(buildUpArgs(&c, "/tmp/k.key", "corp-host"), " ")
	if strings.Contains(args, "SUPERSECRET") {
		t.Fatalf("key leaked into command line: %s", args)
	}
	if !strings.Contains(args, "--auth-key=file:/tmp/k.key") || !strings.Contains(args, "--advertise-tags=tag:endpoint") {
		t.Fatalf("unexpected args: %s", args)
	}
	if got := redact("error with " + c.effectiveAuthKey() + " and tskey-client-SUPERSECRET123"); strings.Contains(got, "SUPERSECRET") {
		t.Fatalf("redaction failed: %s", got)
	}
	if strings.Contains(c.summary(), "SUPERSECRET") {
		t.Fatal("summary must not reveal the key")
	}
}
