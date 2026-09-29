package pipelines

import (
	"strings"
	"testing"
)

func TestExecuteTemplate(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		data    any
		want    string
		wantErr string
	}{
		{name: "field", text: "{{ .Repository.FullName }}@{{ .Revision }}", data: Sample(), want: "octo-org/octo-repo@0123456789abcdef0123456789abcdef01234567"},
		{name: "builtin slice", text: "{{ slice .Revision 0 7 }}", data: Sample(), want: "0123456"},
		{name: "conditional on a nil object", text: "{{ if .Push }}{{ .Push.After }}{{ else }}none{{ end }}", data: SampleFor("pull_request"), want: "none"},
		{name: "nil pull request", text: "{{ .PullRequest.Number }}", data: SampleFor("push"), wantErr: ".PullRequest is only set for pull_request events"},
		{name: "nil push", text: "{{ .Push.Before }}", data: SampleFor("merge_group"), wantErr: ".Push is only set for push events"},
		{name: "nil merge group", text: "{{ .MergeGroup.HeadSHA }}", data: SampleFor("push"), wantErr: ".MergeGroup is only set for merge_group events"},
		{name: "nil comment", text: "{{ .Comment.Arguments }}", data: SampleFor("pull_request"), wantErr: ".Comment is only set for comment events"},
		{name: "nil schedule", text: "{{ .Schedule.Slot }}", data: SampleFor("push"), wantErr: ".Schedule is only set for schedule events"},
		{name: "unknown field", text: "{{ .Commit }}", data: Sample(), wantErr: "can't evaluate field Commit"},
		{name: "missing map key", text: "{{ .missing }}", data: map[string]string{}, wantErr: "map has no entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tpl, err := ParseTemplate(tt.name, tt.text)
			if err != nil {
				t.Fatalf("ParseTemplate: %v", err)
			}
			got, err := ExecuteTemplate(tpl, tt.data)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ExecuteTemplate = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestSampleFor(t *testing.T) {
	tests := []struct {
		event                                            string
		push, pullRequest, mergeGroup, comment, schedule bool
	}{
		{"push", true, false, false, false, false},
		{"pull_request", false, true, false, false, false},
		{"merge_group", false, false, true, false, false},
		{"comment", false, true, false, true, false},
		{"schedule", false, false, false, false, true},
	}
	for _, tt := range tests {
		c := SampleFor(tt.event)
		if c.Event != tt.event || (c.Push != nil) != tt.push || (c.PullRequest != nil) != tt.pullRequest ||
			(c.MergeGroup != nil) != tt.mergeGroup || (c.Comment != nil) != tt.comment || (c.Schedule != nil) != tt.schedule {
			t.Errorf("SampleFor(%s) = %+v", tt.event, c)
		}
	}
	full := Sample()
	if full.Push == nil || full.PullRequest == nil || full.MergeGroup == nil || full.Comment == nil || full.Schedule == nil {
		t.Fatalf("Sample() must populate every event object")
	}
}
