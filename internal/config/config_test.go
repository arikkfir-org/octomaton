package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arikkfir-org/octomatron/internal/tmpl"
)

// referenceConfig is the server configuration from the hub reference.
const referenceConfig = `
github:
  appIDFile: /etc/octomatron/github/app-id
  privateKeyFile: /etc/octomatron/github/private-key
  webhookSecretFile: /etc/octomatron/github/webhook-secret
  allowedOwners: [arikkfir-org]        # installations on other owners are ignored
tekton:
  dashboardURL: https://tekton.kfirs.com
namespaces:
  template: "ci-{{ .Repository.Name }}" # rendered, then sanitized to a DNS label
  overrides:
    arikkfir-org/.github: ci-github
relay:                                  # verified push and pull_request deliveries are forwarded here
  urls: [http://argocd-server.argocd.svc.cluster.local/api/webhook]
retention:
  freePVCsAfter: 1h                     # PVCs of finished runs are deleted after this; runs and pods stay
`

func TestParseReferenceConfig(t *testing.T) {
	cfg, err := Parse([]byte(referenceConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.GitHub.AppIDFile != "/etc/octomatron/github/app-id" || cfg.Tekton.DashboardURL != "https://tekton.kfirs.com" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if !cfg.GitHub.OwnerAllowed("arikkfir-org") || !cfg.GitHub.OwnerAllowed("ARIKKFIR-ORG") || cfg.GitHub.OwnerAllowed("someone-else") {
		t.Fatalf("OwnerAllowed does not follow allowedOwners")
	}
	if len(cfg.Relay.URLs) != 1 || cfg.Retention.FreePVCsAfter.Duration != time.Hour {
		t.Fatalf("relay/retention = %+v / %+v", cfg.Relay, cfg.Retention)
	}
}

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c}
namespaces: {template: "ci-{{ .Repository.Name }}"}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Retention.FreePVCsAfter.Duration != DefaultFreePVCsAfter {
		t.Fatalf("freePVCsAfter default = %v", cfg.Retention.FreePVCsAfter)
	}
	if !cfg.GitHub.OwnerAllowed("anyone") {
		t.Fatalf("without allowedOwners every owner is allowed")
	}
}

func TestParseProblems(t *testing.T) {
	base := "github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c}\nnamespaces: {template: \"ci-{{ .Repository.Name }}\"}\n"
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "empty", yaml: "", want: "configuration is empty"},
		{name: "unknown top-level field", yaml: base + "extra: 1\n", want: "field extra not found"},
		{name: "unknown nested field", yaml: "github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c, typo: x}\nnamespaces: {template: x}\n", want: "field typo not found"},
		{name: "missing files", yaml: "namespaces: {template: x}\n", want: "github.appIDFile is required"},
		{name: "missing template", yaml: "github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c}\n", want: "namespaces.template is required"},
		{name: "bad template", yaml: "github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c}\nnamespaces: {template: \"{{ .Repository.Nope }}\"}\n", want: "can't evaluate field Nope"},
		{name: "template renders nothing usable", yaml: "github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c}\nnamespaces: {template: \"...\"}\n", want: "not usable as a namespace"},
		{name: "bad dashboard URL", yaml: base + "tekton: {dashboardURL: tekton.kfirs.com}\n", want: "tekton.dashboardURL"},
		{name: "bad allowed owner", yaml: "github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c, allowedOwners: [\"a/b\"]}\nnamespaces: {template: x}\n", want: "allowedOwners[0]"},
		{name: "bad override key", yaml: "github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c}\nnamespaces: {template: x, overrides: {justname: ns}}\n", want: `key "justname" must be "owner/name"`},
		{name: "bad override namespace", yaml: "github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c}\nnamespaces: {template: x, overrides: {o/r: Not_Valid}}\n", want: "not a valid namespace name"},
		{name: "bad relay URL", yaml: base + "relay: {urls: [\"argocd/api\"]}\n", want: "relay.urls[0]"},
		{name: "bad retention duration", yaml: base + "retention: {freePVCsAfter: soon}\n", want: "is not a duration"},
		{name: "negative retention", yaml: base + "retention: {freePVCsAfter: -1h}\n", want: "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pemKey(t *testing.T, pkcs8 bool) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	gh := GitHub{
		AppIDFile:         writeFile(t, dir, "app-id", "12345\n"),
		PrivateKeyFile:    writeFile(t, dir, "private-key", pemKey(t, false)),
		WebhookSecretFile: writeFile(t, dir, "webhook-secret", "  hook-secret\n"),
	}
	cfgPath := writeFile(t, dir, "config.yaml", "github:\n  appIDFile: "+gh.AppIDFile+"\n  privateKeyFile: "+gh.PrivateKeyFile+
		"\n  webhookSecretFile: "+gh.WebhookSecretFile+"\nnamespaces:\n  template: \"ci-{{ .Repository.Name }}\"\n")
	cfg, creds, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if creds.AppID != 12345 || creds.PrivateKey == nil || string(creds.WebhookSecret) != "hook-secret" {
		t.Fatalf("credentials = %d, %v, %q", creds.AppID, creds.PrivateKey != nil, creds.WebhookSecret)
	}
	if cfg.Namespaces.Template == "" {
		t.Fatalf("namespaces not loaded")
	}
	if _, _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatalf("Load of a missing file must fail")
	}
}

func TestLoadCredentialsProblems(t *testing.T) {
	dir := t.TempDir()
	key := writeFile(t, dir, "key", pemKey(t, true))
	tests := []struct {
		name string
		gh   GitHub
		want string
	}{
		{name: "PKCS#8 key is accepted", gh: GitHub{AppIDFile: writeFile(t, dir, "id1", "1"), PrivateKeyFile: key, WebhookSecretFile: writeFile(t, dir, "s1", "x")}},
		{name: "app ID not a number", gh: GitHub{AppIDFile: writeFile(t, dir, "id2", "abc"), PrivateKeyFile: key, WebhookSecretFile: writeFile(t, dir, "s2", "x")}, want: "positive GitHub App ID"},
		{name: "missing key file", gh: GitHub{AppIDFile: writeFile(t, dir, "id3", "1"), PrivateKeyFile: filepath.Join(dir, "nope"), WebhookSecretFile: writeFile(t, dir, "s3", "x")}, want: "github.privateKeyFile"},
		{name: "not a PEM key", gh: GitHub{AppIDFile: writeFile(t, dir, "id4", "1"), PrivateKeyFile: writeFile(t, dir, "bad", "hello"), WebhookSecretFile: writeFile(t, dir, "s4", "x")}, want: "no PEM data"},
		{name: "empty webhook secret", gh: GitHub{AppIDFile: writeFile(t, dir, "id5", "1"), PrivateKeyFile: key, WebhookSecretFile: writeFile(t, dir, "s5", "\n")}, want: "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.gh.LoadCredentials()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestSanitizeDNSLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ci-octomatron", "ci-octomatron"},
		{"CI-Docs", "ci-docs"},
		{".github", "github"},
		{"...dots", "dots"},
		{"ci-.github", "ci--github"},
		{"ci-my_repo.name", "ci-my-repo-name"},
		{"ci-a__..b", "ci-a-b"},
		{"-leading-and-trailing-", "leading-and-trailing"},
		{"ci-" + strings.Repeat("x", 80), "ci-" + strings.Repeat("x", 60)},
		{"ci-" + strings.Repeat("x", 59) + "-y", "ci-" + strings.Repeat("x", 59)},
		{"ünïcode", "n-code"},
		{"___", ""},
	}
	for _, tt := range tests {
		if got := SanitizeDNSLabel(tt.in); got != tt.want {
			t.Errorf("SanitizeDNSLabel(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got := SanitizeDNSLabel(tt.in); len(got) > 63 {
			t.Errorf("SanitizeDNSLabel(%q) is longer than 63 characters", tt.in)
		}
	}
}

func TestResolveNamespace(t *testing.T) {
	cfg, err := Parse([]byte(referenceConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tests := []struct {
		repo tmpl.Repository
		want string
	}{
		{tmpl.Repository{Owner: "arikkfir-org", Name: "octomatron", FullName: "arikkfir-org/octomatron"}, "ci-octomatron"},
		{tmpl.Repository{Owner: "arikkfir-org", Name: ".github", FullName: "arikkfir-org/.github"}, "ci-github"},
		{tmpl.Repository{Owner: "Arikkfir-Org", Name: ".GitHub", FullName: "Arikkfir-Org/.GitHub"}, "ci-github"},
		{tmpl.Repository{Owner: "arikkfir-org", Name: "My_Repo", FullName: "arikkfir-org/My_Repo"}, "ci-my-repo"},
	}
	for _, tt := range tests {
		got, err := cfg.Namespaces.Resolve(tt.repo)
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%s) = %q, %v; want %q", tt.repo.FullName, got, err, tt.want)
		}
	}
}
