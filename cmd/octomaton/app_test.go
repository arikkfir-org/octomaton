package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"octomaton.dev/internal/config"
)

// testConfig loads a configuration for a Kubernetes API server that knows no resources.
func testConfig(t *testing.T, address string) *config.Config {
	t.Helper()
	api := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(api.Close)
	kubeconfig := filepath.Join(t.TempDir(), "config")
	data := "apiVersion: v1\nkind: Config\nclusters: [{name: test, cluster: {server: \"" + api.URL + "\"}}]\n" +
		"users: [{name: test, user: {}}]\ncontexts: [{name: test, context: {cluster: test, user: test}}]\ncurrent-context: test\n"
	if err := os.WriteFile(kubeconfig, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"KUBERNETES_SERVICE_HOST":         "",
		"KUBECONFIG":                      kubeconfig,
		"OCTOMATON_GITHUB_APP_ID":         "123",
		"OCTOMATON_GITHUB_PRIVATE_KEY":    string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		"OCTOMATON_GITHUB_WEBHOOK_SECRET": "s3cret",
		"OCTOMATON_HTTP_ADDRESS":          address,
		"OCTOMATON_RELAY_URLS":            "http://127.0.0.1:1/hooks",
		"OCTOMATON_POD_NAME":              "octomaton-0",
		"OCTOMATON_POD_NAMESPACE":         "octomaton",
	} {
		t.Setenv(name, value)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAppRun(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	tests := []struct {
		name     string
		address  string
		cancel   bool
		wantCode int
	}{
		{name: "stops when cancelled", address: "127.0.0.1:0", cancel: true, wantCode: 0},
		{name: "fails when the address is taken", address: taken.Addr().String(), wantCode: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := newApp(testConfig(t, tt.address), http.NotFoundHandler())
			if err != nil {
				t.Fatalf("newApp: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			code := make(chan int, 1)
			go func() { code <- a.run(ctx) }()
			if tt.cancel {
				cancel()
			}
			select {
			case got := <-code:
				if got != tt.wantCode {
					t.Fatalf("run = %d, want %d", got, tt.wantCode)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("run did not return")
			}
		})
	}
}
