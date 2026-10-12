package main

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"octomaton.dev/internal/system/config"
)

// testConfig loads the hub's configuration, serving on address.
func testConfig(t *testing.T, address string) *config.Logs {
	t.Helper()
	for name, value := range map[string]string{
		"OCTOMATON_LOGS_ADDRESS":  address,
		"OCTOMATON_LOGS_VIEW":     "projects/arikkfir/locations/global/buckets/_Default/views/_Default",
		"OCTOMATON_LOGS_CLUSTER":  "hub",
		"OCTOMATON_LOGS_LOCATION": "me-west1-a",
	} {
		t.Setenv(name, value)
	}
	cfg, err := config.LoadLogs()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestServe(t *testing.T) {
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
			cfg := testConfig(t, tt.address)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			code := make(chan int, 1)
			go func() { code <- serve(ctx, cfg, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})) }()
			if tt.cancel {
				cancel()
			}
			select {
			case got := <-code:
				if got != tt.wantCode {
					t.Fatalf("serve = %d, want %d", got, tt.wantCode)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("serve did not return")
			}
		})
	}
}
