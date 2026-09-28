// Package config loads and validates the Octomatron server configuration
// (by default /etc/octomatron/config.yaml) and the GitHub App credentials it points to.
package config

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultPath is where the server configuration is read from unless overridden.
const DefaultPath = "/etc/octomatron/config.yaml"

// DefaultFreePVCsAfter is how long after a PipelineRun finishes its PVCs are deleted.
const DefaultFreePVCsAfter = time.Hour

// Config is the server configuration.
type Config struct {
	GitHub     GitHub     `yaml:"github"`
	Tekton     Tekton     `yaml:"tekton"`
	Namespaces Namespaces `yaml:"namespaces"`
	Relay      Relay      `yaml:"relay"`
	Retention  Retention  `yaml:"retention"`
}

// Relay forwards verified push and pull_request deliveries to other webhook receivers.
type Relay struct {
	URLs []string `yaml:"urls"`
}

// Retention controls what Octomatron cleans up after runs finish.
type Retention struct {
	// FreePVCsAfter is how long after a PipelineRun finished the PVCs it owns are
	// deleted (default 1h). Pods are kept: the Tekton Dashboard reads logs from them.
	FreePVCsAfter Duration `yaml:"freePVCsAfter"`
}

// Duration is a time.Duration written as a Go duration string ("90m", "1h").
type Duration struct {
	time.Duration
}

// UnmarshalYAML parses a duration string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (examples: 30m, 1h)", node.Line, s)
	}
	d.Duration = parsed
	return nil
}

// GitHub configures the GitHub App.
type GitHub struct {
	AppIDFile         string   `yaml:"appIDFile"`
	PrivateKeyFile    string   `yaml:"privateKeyFile"`
	WebhookSecretFile string   `yaml:"webhookSecretFile"`
	AllowedOwners     []string `yaml:"allowedOwners"`
}

// Tekton configures links to Tekton.
type Tekton struct {
	DashboardURL string `yaml:"dashboardURL"`
}

// Credentials are the GitHub App secrets read from the files named in GitHub.
type Credentials struct {
	AppID         int64
	PrivateKey    *rsa.PrivateKey
	WebhookSecret []byte
}

// Error lists every problem found in a configuration.
type Error struct {
	Source   string
	Problems []string
}

func (e *Error) Error() string {
	return fmt.Sprintf("invalid configuration %s:\n  - %s", e.Source, strings.Join(e.Problems, "\n  - "))
}

// Load reads, parses and validates the configuration at path and the credentials it references.
func Load(path string) (*Config, *Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading configuration: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		var ce *Error
		if errors.As(err, &ce) {
			ce.Source = path
		}
		return nil, nil, err
	}
	creds, err := cfg.GitHub.LoadCredentials()
	if err != nil {
		return nil, nil, err
	}
	return cfg, creds, nil
}

// Parse decodes (rejecting unknown fields) and validates a configuration document.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &Error{Source: "(input)", Problems: []string{"configuration is empty"}}
		}
		return nil, &Error{Source: "(input)", Problems: []string{err.Error()}}
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if cfg.GitHub.AppIDFile == "" {
		add("github.appIDFile is required")
	}
	if cfg.GitHub.PrivateKeyFile == "" {
		add("github.privateKeyFile is required")
	}
	if cfg.GitHub.WebhookSecretFile == "" {
		add("github.webhookSecretFile is required")
	}
	for i, owner := range cfg.GitHub.AllowedOwners {
		if strings.TrimSpace(owner) == "" || strings.Contains(owner, "/") {
			add("github.allowedOwners[%d]: %q is not a GitHub user or organization name", i, owner)
		}
	}
	if u := cfg.Tekton.DashboardURL; u != "" {
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			add("tekton.dashboardURL: %q is not an absolute http(s) URL", u)
		}
		cfg.Tekton.DashboardURL = strings.TrimRight(u, "/")
	}
	problems = append(problems, cfg.Namespaces.init()...)
	for i, u := range cfg.Relay.URLs {
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			add("relay.urls[%d]: %q is not an absolute http(s) URL", i, u)
		}
	}
	switch {
	case cfg.Retention.FreePVCsAfter.Duration < 0:
		add("retention.freePVCsAfter must not be negative")
	case cfg.Retention.FreePVCsAfter.Duration == 0:
		cfg.Retention.FreePVCsAfter.Duration = DefaultFreePVCsAfter
	}
	if len(problems) > 0 {
		return nil, &Error{Source: "(input)", Problems: problems}
	}
	return &cfg, nil
}

// OwnerAllowed reports whether events from repositories owned by owner are processed.
func (g GitHub) OwnerAllowed(owner string) bool {
	if len(g.AllowedOwners) == 0 {
		return true
	}
	for _, allowed := range g.AllowedOwners {
		if strings.EqualFold(allowed, owner) {
			return true
		}
	}
	return false
}

// LoadCredentials reads and validates the App ID, private key and webhook secret files.
func (g GitHub) LoadCredentials() (*Credentials, error) {
	var problems []string
	creds := &Credentials{}

	if raw, err := os.ReadFile(g.AppIDFile); err != nil {
		problems = append(problems, fmt.Sprintf("github.appIDFile: %v", err))
	} else if id, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err != nil || id <= 0 {
		problems = append(problems, fmt.Sprintf("github.appIDFile: %s does not contain a positive GitHub App ID", g.AppIDFile))
	} else {
		creds.AppID = id
	}

	if raw, err := os.ReadFile(g.PrivateKeyFile); err != nil {
		problems = append(problems, fmt.Sprintf("github.privateKeyFile: %v", err))
	} else if key, err := ParsePrivateKey(raw); err != nil {
		problems = append(problems, fmt.Sprintf("github.privateKeyFile: %s: %v", g.PrivateKeyFile, err))
	} else {
		creds.PrivateKey = key
	}

	if raw, err := os.ReadFile(g.WebhookSecretFile); err != nil {
		problems = append(problems, fmt.Sprintf("github.webhookSecretFile: %v", err))
	} else if secret := bytes.TrimSpace(raw); len(secret) == 0 {
		problems = append(problems, fmt.Sprintf("github.webhookSecretFile: %s is empty", g.WebhookSecretFile))
	} else {
		creds.WebhookSecret = secret
	}

	if len(problems) > 0 {
		return nil, &Error{Source: "(GitHub credentials)", Problems: problems}
	}
	return creds, nil
}

// ParsePrivateKey parses a PEM-encoded RSA private key (PKCS#1, as GitHub issues
// them, or PKCS#8).
func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM data found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not an RSA private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return key, nil
}
