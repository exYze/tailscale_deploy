package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// embeddedConfigJSON is replaced by build.ps1 at build time with the contents of
// deploy-config.json (and restored to "{}" afterwards).
//
//go:embed embedded_config.json
var embeddedConfigJSON []byte

// Config is the deployment configuration baked into the binary.
type Config struct {
	// AuthKey is a Tailscale auth key (tskey-auth-...) or an OAuth client
	// secret (tskey-client-...). OAuth secrets do not expire and are the
	// recommended choice for long-lived installers.
	AuthKey string `json:"authKey"`
	// Tags to advertise, e.g. ["tag:endpoint"]. Required for OAuth secrets.
	Tags stringList `json:"tags"`
	// OAuthPreauthorized skips device approval for OAuth-registered devices.
	// Defaults to true. Ignored for regular auth keys (set that when creating the key).
	OAuthPreauthorized *bool `json:"oauthPreauthorized,omitempty"`
	// LoginServer is a custom coordination server (e.g. Headscale). Empty = Tailscale.
	LoginServer string `json:"loginServer,omitempty"`
	// HostnamePrefix is prepended to the machine's hostname, e.g. "corp-".
	HostnamePrefix string `json:"hostnamePrefix,omitempty"`
	// Version is "latest" (default) or a pinned version like "1.104.5".
	Version string `json:"version,omitempty"`
	// Track is "stable" (default) or "unstable".
	Track string `json:"track,omitempty"`
	// MirrorURL is an internal HTTP(S) mirror of pkgs.tailscale.com/<track>.
	// It must serve the installer files and their .sha256 files. Requires a pinned Version.
	MirrorURL string `json:"mirrorURL,omitempty"`
	// AcceptRoutes accepts subnet routes advertised by other devices.
	AcceptRoutes bool `json:"acceptRoutes,omitempty"`
	// AcceptDNS overrides Tailscale's DNS setting (nil = Tailscale default).
	AcceptDNS *bool `json:"acceptDNS,omitempty"`
	// ForceReauth re-enrolls devices that are already connected to a tailnet.
	ForceReauth bool `json:"forceReauth,omitempty"`
	// ConnectTimeoutSeconds bounds each wait (service start, connect). Default 180.
	ConnectTimeoutSeconds int `json:"connectTimeoutSeconds,omitempty"`
	// ExtraUpArgs are appended to "tailscale up", e.g. ["--ssh"].
	ExtraUpArgs []string `json:"extraUpArgs,omitempty"`
	// WindowsMSIProperties are passed to msiexec, e.g. {"TS_INSTALLUPDATES":"always"}.
	// TS_UNATTENDEDMODE defaults to "always".
	WindowsMSIProperties map[string]string `json:"windowsMSIProperties,omitempty"`
	// LinuxUseInstallScript uses Tailscale's official install.sh (package-manager
	// install with updates) before falling back to static binaries. Default true.
	LinuxUseInstallScript *bool `json:"linuxUseInstallScript,omitempty"`
}

// stringList accepts either a JSON array of strings or a single
// comma/space-separated string.
type stringList []string

func (s *stringList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = splitList(one)
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("expected a string or an array of strings: %w", err)
	}
	out := stringList{}
	for _, m := range many {
		out = append(out, splitList(m)...)
	}
	*s = out
	return nil
}

func splitList(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' })
	out := make([]string, 0, len(f))
	for _, x := range f {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func loadConfig(b []byte) (*Config, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // tolerate a UTF-8 BOM
	b = bytes.TrimSpace(b)
	c := &Config{}
	if len(b) == 0 {
		return c, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // catch typos like "tag" instead of "tags"
	if err := dec.Decode(c); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyOverrides(o options, envKey string) {
	if envKey = strings.TrimSpace(envKey); envKey != "" {
		c.AuthKey = envKey
	}
	if o.authKey != "" {
		c.AuthKey = o.authKey
	}
	if o.tags != "" {
		c.Tags = splitList(o.tags)
	}
	if o.hostnamePrefix != "" {
		c.HostnamePrefix = o.hostnamePrefix
	}
	if o.loginServer != "" {
		c.LoginServer = o.loginServer
	}
	if o.forceReauth {
		c.ForceReauth = true
	}
}

var (
	versionRe     = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	msiPropNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

func (c *Config) normalize() error {
	c.AuthKey = strings.TrimSpace(c.AuthKey)
	c.LoginServer = strings.TrimSpace(c.LoginServer)
	if c.AuthKey == "" {
		return errors.New("no auth key is embedded in this binary: build it with build.ps1, or pass --authkey (or the TS_DEPLOY_AUTHKEY environment variable)")
	}
	if c.LoginServer == "" && !strings.HasPrefix(c.AuthKey, "tskey-") {
		return errors.New(`the auth key must start with "tskey-" (an auth key "tskey-auth-..." or an OAuth client secret "tskey-client-...")`)
	}
	for _, t := range c.Tags {
		if !strings.HasPrefix(t, "tag:") || len(t) < 5 {
			return fmt.Errorf("invalid tag %q: tags look like tag:endpoint", t)
		}
	}
	if c.isOAuth() && len(c.Tags) == 0 {
		return errors.New(`an OAuth client secret needs at least one tag (set "tags", for example ["tag:endpoint"])`)
	}

	c.Version = strings.TrimPrefix(strings.TrimSpace(c.Version), "v")
	if c.Version == "" {
		c.Version = "latest"
	}
	if c.Version != "latest" && !versionRe.MatchString(c.Version) {
		return fmt.Errorf(`invalid version %q: use "latest" or a version like "1.104.5"`, c.Version)
	}
	c.Track = strings.ToLower(strings.TrimSpace(c.Track))
	if c.Track == "" {
		c.Track = "stable"
	}
	if c.Track != "stable" && c.Track != "unstable" {
		return fmt.Errorf(`invalid track %q: use "stable" or "unstable"`, c.Track)
	}
	c.MirrorURL = strings.TrimRight(strings.TrimSpace(c.MirrorURL), "/")
	if c.MirrorURL != "" {
		if !strings.HasPrefix(c.MirrorURL, "https://") && !strings.HasPrefix(c.MirrorURL, "http://") {
			return fmt.Errorf("mirrorURL %q must start with https:// or http://", c.MirrorURL)
		}
		if c.Version == "latest" {
			return errors.New(`mirrorURL requires a pinned "version" (for example "1.104.5")`)
		}
	}
	if c.ConnectTimeoutSeconds <= 0 {
		c.ConnectTimeoutSeconds = 180
	}
	if c.WindowsMSIProperties == nil {
		c.WindowsMSIProperties = map[string]string{}
	}
	for k := range c.WindowsMSIProperties {
		if !msiPropNameRe.MatchString(k) {
			return fmt.Errorf("invalid MSI property name %q (use names like TS_UNATTENDEDMODE)", k)
		}
	}
	if _, ok := c.WindowsMSIProperties["TS_UNATTENDEDMODE"]; !ok {
		c.WindowsMSIProperties["TS_UNATTENDEDMODE"] = "always"
	}
	for _, a := range c.ExtraUpArgs {
		if !strings.HasPrefix(a, "--") {
			return fmt.Errorf("extraUpArgs entry %q must be a flag starting with --", a)
		}
		name := strings.SplitN(strings.TrimPrefix(a, "--"), "=", 2)[0]
		if name == "authkey" || name == "auth-key" {
			return errors.New(`put the key in "authKey", not in extraUpArgs`)
		}
	}
	return nil
}

func (c *Config) isOAuth() bool { return strings.HasPrefix(c.AuthKey, "tskey-client-") }

// effectiveAuthKey is the value handed to `tailscale up --auth-key`. OAuth
// secrets get the parameters that make devices permanent (not ephemeral) and,
// by default, pre-approved.
func (c *Config) effectiveAuthKey() string {
	if c.isOAuth() && !strings.Contains(c.AuthKey, "?") {
		pre := true
		if c.OAuthPreauthorized != nil {
			pre = *c.OAuthPreauthorized
		}
		return c.AuthKey + "?ephemeral=false&preauthorized=" + strconv.FormatBool(pre)
	}
	return c.AuthKey
}

// secrets lists strings that must never appear in logs.
func (c *Config) secrets() []string {
	base := strings.SplitN(c.AuthKey, "?", 2)[0]
	return []string{c.effectiveAuthKey(), c.AuthKey, base}
}

func (c *Config) baseURL() string {
	if c.MirrorURL != "" {
		return c.MirrorURL
	}
	return "https://pkgs.tailscale.com/" + c.Track
}

// summary describes the config without revealing the key.
func (c *Config) summary() string {
	kind := "auth key"
	if c.isOAuth() {
		kind = "OAuth client secret (does not expire)"
	}
	tail := c.AuthKey
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	tags := "(none)"
	if len(c.Tags) > 0 {
		tags = strings.Join(c.Tags, ",")
	}
	src := c.baseURL()
	server := "Tailscale"
	if c.LoginServer != "" {
		server = c.LoginServer
	}
	return fmt.Sprintf("key: %s ending ...%s | tags: %s | version: %s (%s) | packages: %s | control server: %s",
		kind, tail, tags, c.Version, c.Track, src, server)
}
