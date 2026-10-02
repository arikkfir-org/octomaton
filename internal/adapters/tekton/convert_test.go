package tekton

import (
	"reflect"
	"testing"

	"octomaton.dev/internal/services/ci"
)

// TestTokenSettingsOf covers the token settings a run keeps in its annotation, written before and
// after tokens for every repository existed.
func TestTokenSettingsOf(t *testing.T) {
	tests := []struct {
		name, raw string
		want      *ci.TokenSettings
	}{
		{"none", "", nil},
		{"unreadable", "{", nil},
		{
			"the run's repository", `{"workspace":"github-token","permissions":{"contents":"read"}}`,
			&ci.TokenSettings{Workspace: "github-token", Permissions: map[string]string{"contents": "read"}},
		},
		{
			"every repository", `{"workspace":"github-token","permissions":{"contents":"read","pull_requests":"read"},"allRepositories":true}`,
			&ci.TokenSettings{Workspace: "github-token", Permissions: map[string]string{"contents": "read", "pull_requests": "read"}, AllRepositories: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenSettingsOf(tt.raw); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("tokenSettingsOf(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}
