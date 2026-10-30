package config

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// setReferenceEnv sets variables like those the hub's Deployment octomaton sets (deploy/).
func setReferenceEnv(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"OCTOMATON_GITHUB_APP_ID":             "123\n",
		"OCTOMATON_GITHUB_PRIVATE_KEY":        testKeyPEM(t),
		"OCTOMATON_GITHUB_WEBHOOK_SECRET":     "s3cret\n",
		"OCTOMATON_GITHUB_ALLOWED_OWNERS":     "arikkfir-org",
		"OCTOMATON_TEKTON_DASHBOARD_URL":      "https://tekton.dev.kfirs.com/",
		"OCTOMATON_ORGANIZATION_REPOSITORY":   "tooling",
		"OCTOMATON_RELAY_URLS":                "http://argocd-server.argocd.svc.cluster.local/api/webhook",
		"OCTOMATON_RETENTION_FREE_PVCS_AFTER": "90m",
		"OCTOMATON_POD_NAME":                  "octomaton-abc",
		"OCTOMATON_POD_NAMESPACE":             "octomaton",
	} {
		t.Setenv(name, value)
	}
}

func TestLoadReference(t *testing.T) {
	setReferenceEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHub.AppID != 123 || cfg.GitHub.Key() == nil || !slices.EqualFunc(cfg.GitHub.WebhookSecrets(), [][]byte{[]byte("s3cret")}, bytes.Equal) {
		t.Errorf("GitHub = %+v", cfg.GitHub)
	}
	if !slices.Equal(cfg.GitHub.AllowedOwners, []string{"arikkfir-org"}) {
		t.Errorf("AllowedOwners = %q", cfg.GitHub.AllowedOwners)
	}
	if cfg.Tekton.DashboardURL != "https://tekton.dev.kfirs.com" {
		t.Errorf("DashboardURL = %q (trailing slash kept?)", cfg.Tekton.DashboardURL)
	}
	if len(cfg.Relay.URLs) != 1 || cfg.Retention.FreePVCsAfter != 90*time.Minute {
		t.Errorf("Relay = %v, Retention = %v", cfg.Relay, cfg.Retention)
	}
	if cfg.Pod != (Pod{Name: "octomaton-abc", Namespace: "octomaton"}) {
		t.Errorf("Pod = %+v", cfg.Pod)
	}
	if cfg.Organization.Repository != "tooling" {
		t.Errorf("Organization = %+v", cfg.Organization)
	}
}

func TestLoadWebhookSecrets(t *testing.T) {
	tests := []struct {
		name, value string
		want        []string
	}{
		{name: "one secret", value: "s3cret", want: []string{"s3cret"}},
		{name: "one secret with a trailing newline", value: "s3cret\n", want: []string{"s3cret"}},
		{name: "one secret with surrounding whitespace", value: " s3cret \r\n", want: []string{"s3cret"}},
		{name: "a secret with a space", value: "s3 cret", want: []string{"s3 cret"}},
		{name: "two secrets", value: "previous\nnext\n", want: []string{"previous", "next"}},
		{name: "two secrets with CRLF", value: "previous\r\nnext\r\n", want: []string{"previous", "next"}},
		{name: "empty lines are skipped", value: "\nprevious\n\n \r\nnext", want: []string{"previous", "next"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setReferenceEnv(t)
			t.Setenv("OCTOMATON_GITHUB_WEBHOOK_SECRET", tt.value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var got []string
			for _, secret := range cfg.GitHub.WebhookSecrets() {
				got = append(got, string(secret))
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("WebhookSecrets() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	setReferenceEnv(t)
	for _, name := range []string{"OCTOMATON_RETENTION_FREE_PVCS_AFTER", "OCTOMATON_POD_NAME", "OCTOMATON_POD_NAMESPACE", "OCTOMATON_ORGANIZATION_REPOSITORY"} {
		t.Setenv(name, "") // restores the variable after the test
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Address != ":8080" {
		t.Errorf("HTTP = %+v", cfg.HTTP)
	}
	if cfg.Webhook.Workers != 8 || cfg.Webhook.QueueSize != 256 || cfg.Namespaces.Template != "ci-{{ .Repository.Name }}" {
		t.Errorf("Webhook = %+v, Namespaces.Template = %q", cfg.Webhook, cfg.Namespaces.Template)
	}
	if cfg.Pod.Name == "" || cfg.Pod.Namespace == "" {
		t.Errorf("Pod defaults = %+v", cfg.Pod)
	}
	if cfg.Organization.Repository != "" {
		t.Errorf("Organization = %+v, want none (no organization pipelines)", cfg.Organization)
	}
}

func TestLoadProblems(t *testing.T) {
	tests := []struct {
		name, variable, value, want string
	}{
		{name: "empty app ID", variable: "OCTOMATON_GITHUB_APP_ID", value: "", want: "OCTOMATON_GITHUB_APP_ID"},
		{name: "non-numeric app ID", variable: "OCTOMATON_GITHUB_APP_ID", value: "octomaton", want: "OCTOMATON_GITHUB_APP_ID"},
		{name: "negative app ID", variable: "OCTOMATON_GITHUB_APP_ID", value: "-1", want: "positive GitHub App ID"},
		{name: "bad private key", variable: "OCTOMATON_GITHUB_PRIVATE_KEY", value: "not a key", want: "OCTOMATON_GITHUB_PRIVATE_KEY: no PEM data"},
		{name: "blank webhook secret", variable: "OCTOMATON_GITHUB_WEBHOOK_SECRET", value: " \n", want: "OCTOMATON_GITHUB_WEBHOOK_SECRET is required"},
		{name: "webhook secret of empty lines", variable: "OCTOMATON_GITHUB_WEBHOOK_SECRET", value: "\r\n\n \n", want: "OCTOMATON_GITHUB_WEBHOOK_SECRET is required"},
		{name: "no owners", variable: "OCTOMATON_GITHUB_ALLOWED_OWNERS", value: "", want: "OCTOMATON_GITHUB_ALLOWED_OWNERS is required"},
		{name: "repository as owner", variable: "OCTOMATON_GITHUB_ALLOWED_OWNERS", value: "arikkfir-org/docs", want: "not a GitHub user or organization"},
		{name: "relative dashboard URL", variable: "OCTOMATON_TEKTON_DASHBOARD_URL", value: "tekton.dev.kfirs.com", want: "OCTOMATON_TEKTON_DASHBOARD_URL"},
		{name: "bad relay URL", variable: "OCTOMATON_RELAY_URLS", value: "ftp://x", want: "OCTOMATON_RELAY_URLS"},
		{name: "zero retention", variable: "OCTOMATON_RETENTION_FREE_PVCS_AFTER", value: "0s", want: "must be positive"},
		{name: "no workers", variable: "OCTOMATON_WEBHOOK_WORKERS", value: "0", want: "must be positive"},
		{name: "bad duration", variable: "OCTOMATON_RETENTION_FREE_PVCS_AFTER", value: "soon", want: "OCTOMATON_RETENTION_FREE_PVCS_AFTER"},
		{name: "organization repository with its owner", variable: "OCTOMATON_ORGANIZATION_REPOSITORY", value: "arikkfir-org/tooling", want: `OCTOMATON_ORGANIZATION_REPOSITORY: "arikkfir-org/tooling" is not a repository name`},
		{name: "organization repository ..", variable: "OCTOMATON_ORGANIZATION_REPOSITORY", value: "..", want: "is not a repository name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setReferenceEnv(t)
			t.Setenv(tt.variable, tt.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadListsEveryMissingCredential(t *testing.T) {
	setReferenceEnv(t)
	for _, name := range []string{"OCTOMATON_GITHUB_APP_ID", "OCTOMATON_GITHUB_PRIVATE_KEY", "OCTOMATON_GITHUB_WEBHOOK_SECRET", "OCTOMATON_GITHUB_ALLOWED_OWNERS"} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Load()
	for _, want := range []string{"OCTOMATON_GITHUB_APP_ID is required", "OCTOMATON_GITHUB_PRIVATE_KEY is required", "OCTOMATON_GITHUB_WEBHOOK_SECRET is required", "OCTOMATON_GITHUB_ALLOWED_OWNERS is required"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %v, want it to say %q", err, want)
		}
	}
}

func TestLoadNeverPrintsThePrivateKey(t *testing.T) {
	setReferenceEnv(t)
	t.Setenv("OCTOMATON_GITHUB_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----\nc2VjcmV0\n-----END RSA PRIVATE KEY-----\n")
	_, err := Load()
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Fatalf("Load() error = %v", err)
	}
}
