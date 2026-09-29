package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octomaton.dev/internal/buildinfo"
)

func TestRun(t *testing.T) {
	valid := t.TempDir()
	write := func(dir, name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(valid, ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines:\n"+
		"  - {name: ci, pipelineRun: run.yaml, on: {push: {branches: [main]}}, params: {revision: \"{{ .Revision }}\"}}\n")
	write(valid, "run.yaml", "apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec: {pipelineRef: {name: p}}\n")
	invalid := t.TempDir()
	write(invalid, ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: [{name: ci}]\n")

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"-version"}, wantCode: 0, wantStdout: buildinfo.Version() + "\n"},
		{name: "help", args: []string{"-h"}, wantCode: 0, wantStderr: "Usage: octomaton-lint"},
		{name: "no paths", args: nil, wantCode: 2, wantStderr: "Usage: octomaton-lint"},
		{name: "unknown flag", args: []string{"-bogus", valid}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "valid configuration", args: []string{valid}, wantCode: 0, wantStdout: "ok (1 pipeline runs rendered)"},
		{name: "render", args: []string{"-render", valid}, wantCode: 0, wantStdout: "kind: PipelineRun"},
		{name: "invalid configuration", args: []string{invalid}, wantCode: 1, wantStderr: ".octomaton.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode || !strings.Contains(stdout.String(), tt.wantStdout) || !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Fatalf("run(%q) = %d\nstdout: %q\nstderr: %q", tt.args, code, stdout.String(), stderr.String())
			}
		})
	}
}
