package config

import (
	"os"
	"strings"
	"testing"
)

// setLogsReferenceEnv sets the variables of the hub reference's Deployment octomaton-logs.
func setLogsReferenceEnv(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"OCTOMATON_LOGS_VIEW":     "projects/arikkfir/locations/global/buckets/_Default/views/_Default",
		"OCTOMATON_LOGS_CLUSTER":  "hub",
		"OCTOMATON_LOGS_LOCATION": "me-west1-a",
	} {
		t.Setenv(name, value)
	}
}

func TestLoadLogs(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Logs
	}{
		{name: "reference, with the defaults", want: Logs{
			Address: ":8080", View: "projects/arikkfir/locations/global/buckets/_Default/views/_Default",
			Cluster: "hub", Location: "me-west1-a", NamespacePrefix: "ci-", project: "arikkfir",
		}},
		{name: "every variable", env: map[string]string{
			"OCTOMATON_LOGS_ADDRESS":          "127.0.0.1:9090",
			"OCTOMATON_LOGS_VIEW":             "projects/other-project/locations/me-west1/buckets/ci/views/containers",
			"OCTOMATON_LOGS_NAMESPACE_PREFIX": "tenant-",
		}, want: Logs{
			Address: "127.0.0.1:9090", View: "projects/other-project/locations/me-west1/buckets/ci/views/containers",
			Cluster: "hub", Location: "me-west1-a", NamespacePrefix: "tenant-", project: "other-project",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setLogsReferenceEnv(t)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			cfg, err := LoadLogs()
			if err != nil {
				t.Fatalf("LoadLogs: %v", err)
			}
			if *cfg != tt.want {
				t.Fatalf("LoadLogs = %+v, want %+v", *cfg, tt.want)
			}
			if cfg.Project() != tt.want.project {
				t.Fatalf("Project = %q, want %q", cfg.Project(), tt.want.project)
			}
		})
	}
}

func TestLoadLogsProblems(t *testing.T) {
	tests := []struct {
		name, variable, value, want string
	}{
		{name: "no view", variable: "OCTOMATON_LOGS_VIEW", value: "", want: "OCTOMATON_LOGS_VIEW is required"},
		{name: "a bucket, not a view", variable: "OCTOMATON_LOGS_VIEW", value: "projects/arikkfir/locations/global/buckets/_Default",
			want: `OCTOMATON_LOGS_VIEW: "projects/arikkfir/locations/global/buckets/_Default" is not a log view`},
		{name: "a view without its project", variable: "OCTOMATON_LOGS_VIEW", value: "locations/global/buckets/_Default/views/_Default",
			want: "is not a log view"},
		{name: "a view with a filter's quote", variable: "OCTOMATON_LOGS_VIEW", value: `projects/arikkfir/locations/global/buckets/_Default/views/"`,
			want: "is not a log view"},
		{name: "no cluster", variable: "OCTOMATON_LOGS_CLUSTER", value: "", want: "OCTOMATON_LOGS_CLUSTER is required"},
		{name: "a cluster that would leave its quotes", variable: "OCTOMATON_LOGS_CLUSTER", value: `hub" OR "x`,
			want: `OCTOMATON_LOGS_CLUSTER: "hub\" OR \"x" is not a GKE cluster name`},
		{name: "no location", variable: "OCTOMATON_LOGS_LOCATION", value: "", want: "OCTOMATON_LOGS_LOCATION is required"},
		{name: "upper-case location", variable: "OCTOMATON_LOGS_LOCATION", value: "ME-WEST1-A", want: "is not a GKE location"},
		{name: "empty namespace prefix", variable: "OCTOMATON_LOGS_NAMESPACE_PREFIX", value: "", want: "OCTOMATON_LOGS_NAMESPACE_PREFIX must not be empty"},
		{name: "namespace prefix with a slash", variable: "OCTOMATON_LOGS_NAMESPACE_PREFIX", value: "ci/",
			want: `OCTOMATON_LOGS_NAMESPACE_PREFIX: "ci/" does not start a namespace name`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setLogsReferenceEnv(t)
			t.Setenv(tt.variable, tt.value)
			_, err := LoadLogs()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadLogs() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadLogsListsEveryMissingVariable(t *testing.T) {
	setLogsReferenceEnv(t)
	for _, name := range []string{"OCTOMATON_LOGS_VIEW", "OCTOMATON_LOGS_CLUSTER", "OCTOMATON_LOGS_LOCATION"} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	_, err := LoadLogs()
	for _, want := range []string{"OCTOMATON_LOGS_VIEW is required", "OCTOMATON_LOGS_CLUSTER is required", "OCTOMATON_LOGS_LOCATION is required"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadLogs() error = %v, want it to say %q", err, want)
		}
	}
}
