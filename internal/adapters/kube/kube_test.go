package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// useKubeconfig points the clients at server, outside any cluster the tests may run in.
func useKubeconfig(t *testing.T, server string) {
	t.Helper()
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	kubeconfig := filepath.Join(t.TempDir(), "config")
	data := `apiVersion: v1
kind: Config
clusters: [{name: test, cluster: {server: "` + server + `"}}]
users: [{name: test, user: {}}]
contexts: [{name: test, context: {cluster: test, user: test}}]
current-context: test
`
	if err := os.WriteFile(kubeconfig, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
}

func TestPing(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "API server answers", status: http.StatusOK},
		{name: "API server fails", status: http.StatusInternalServerError, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var userAgent string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				userAgent = r.UserAgent()
				if r.URL.Path != "/version" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"major":"1","minor":"34"}`))
			}))
			defer server.Close()
			useKubeconfig(t, server.URL)

			clients, err := NewClients("octomaton/v0.0.0-test")
			if err != nil {
				t.Fatalf("NewClients: %v", err)
			}
			err = clients.Ping(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Ping error = %v, want error %v", err, tt.wantErr)
			}
			if userAgent != "octomaton/v0.0.0-test" {
				t.Fatalf("User-Agent = %q", userAgent)
			}
		})
	}
}

func TestNewClientsWithoutConfiguration(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("HOME", t.TempDir())
	if _, err := NewClients("octomaton/test"); err == nil {
		t.Fatal("NewClients succeeded without any cluster configuration")
	}
}
