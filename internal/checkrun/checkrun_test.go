package checkrun

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func sampleContext() Context {
	return Context{
		Version:        ContextVersion,
		Event:          EventPullRequest,
		Action:         "synchronize",
		DeliveryID:     "d-1",
		InstallationID: 7,
		Repository:     Repository{ID: 42, Owner: "octo", Name: "repo", FullName: "octo/repo", CloneURL: "https://github.com/octo/repo.git", DefaultBranch: "main"},
		Revision:       "0123456789abcdef0123456789abcdef01234567",
		Ref:            "refs/pull/5/head",
		Branch:         "feature",
		Sender:         "alice",
		Pipeline:       "ci",
		PullRequest:    &PullRequest{Number: 5, HeadRef: "feature", HeadSHA: "0123456789abcdef0123456789abcdef01234567", BaseRef: "main", BaseSHA: "abc", HeadRepo: "fork/repo", AuthorAssociation: "NONE"},
		ApprovedBy:     "bob",
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	c := sampleContext()
	m, err := Marker(c)
	if err != nil {
		t.Fatalf("Marker: %v", err)
	}
	if !strings.HasPrefix(m, "<!-- octomatron:context:") || !strings.HasSuffix(m, " -->") || strings.Count(m, "-->") != 1 {
		t.Fatalf("marker is not a single HTML comment: %q", m)
	}
	got, found, err := DecodeMarker("Some text\n\n" + m)
	if err != nil || !found {
		t.Fatalf("DecodeMarker: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, c)
	}
}

func TestMarkerSetsVersion(t *testing.T) {
	c := sampleContext()
	c.Version = 0
	got, found, err := DecodeMarker(MustMarker(c))
	if err != nil || !found || got.Version != ContextVersion {
		t.Fatalf("DecodeMarker = %+v, %v, %v", got, found, err)
	}
}

func TestDecodeMarkerProblems(t *testing.T) {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tests := []struct {
		name      string
		text      string
		wantFound bool
		wantErr   string
	}{
		{name: "no marker", text: "plain text"},
		{name: "unterminated", text: "<!-- octomatron:context:abc", wantFound: true, wantErr: "unterminated"},
		{name: "bad base64", text: "<!-- octomatron:context:!!! -->", wantFound: true, wantErr: "decoding"},
		{name: "bad JSON", text: "<!-- octomatron:context:" + enc("nope") + " -->", wantFound: true, wantErr: "parsing"},
		{name: "unknown version", text: "<!-- octomatron:context:" + enc(`{"v":99}`) + " -->", wantFound: true, wantErr: "unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, found, err := DecodeMarker(tt.text)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeMarkerUsesTheLastMarker(t *testing.T) {
	first, second := sampleContext(), sampleContext()
	second.Pipeline = "release"
	got, _, err := DecodeMarker(MustMarker(first) + "\n" + MustMarker(second))
	if err != nil || got.Pipeline != "release" {
		t.Fatalf("got pipeline %q, %v", got.Pipeline, err)
	}
}

func TestWithMarkerKeepsTheMarkerWithinTheLimit(t *testing.T) {
	c := sampleContext()
	long := strings.Repeat("log line ✓\n", 20000)
	text := WithMarker(long, c)
	if len(text) > MaxOutputLength {
		t.Fatalf("text is %d bytes, over the limit", len(text))
	}
	if !utf8.ValidString(text) {
		t.Fatalf("truncation split a UTF-8 sequence")
	}
	got, found, err := DecodeMarker(text)
	if !found || err != nil || got.Pipeline != "ci" {
		t.Fatalf("marker lost in truncated text: %v %v", found, err)
	}
	if WithMarker("", c) != MustMarker(c) {
		t.Fatalf("an empty text is just the marker")
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Fatalf("Truncate kept %q", got)
	}
	got := Truncate(strings.Repeat("é", 100), 50)
	if len(got) > 50 || !utf8.ValidString(got) || !strings.HasSuffix(got, "_(truncated)_") {
		t.Fatalf("Truncate = %q (%d bytes)", got, len(got))
	}
	if got := TruncateHead("abcdef", 3); got != "def" {
		t.Fatalf("TruncateHead = %q", got)
	}
	if got := TruncateHead("ééé", 3); !utf8.ValidString(got) || len(got) > 3 {
		t.Fatalf("TruncateHead split a rune: %q", got)
	}
}

func TestDashboardURLs(t *testing.T) {
	if got := DashboardURL("https://tekton.dev.kfirs.com/", "ci-docs", "docs-ci-abc1234-1"); got != "https://tekton.dev.kfirs.com/#/namespaces/ci-docs/pipelineruns/docs-ci-abc1234-1" {
		t.Fatalf("DashboardURL = %q", got)
	}
	if got := TaskRunURL("https://tekton.dev.kfirs.com", "ns", "tr"); got != "https://tekton.dev.kfirs.com/#/namespaces/ns/taskruns/tr" {
		t.Fatalf("TaskRunURL = %q", got)
	}
	if DashboardURL("", "ns", "run") != "" {
		t.Fatalf("no dashboard, no link")
	}
}

func TestContextHelpers(t *testing.T) {
	c := sampleContext()
	tc := c.Template()
	if tc.PullRequest == nil || tc.PullRequest.Number != 5 || tc.Push != nil || tc.Repository.FullName != "octo/repo" || tc.Pipeline != "ci" {
		t.Fatalf("Template() = %+v", tc)
	}
	if c.ConfigAt() != c.Revision {
		t.Fatalf("ConfigAt defaults to the revision")
	}
	c.ConfigRef = "main"
	if c.ConfigAt() != "main" {
		t.Fatalf("ConfigAt must prefer ConfigRef")
	}
	if c.Head() != "feature" {
		t.Fatalf("Head() = %q", c.Head())
	}
	tag := Context{Tag: "v1", Branch: ""}
	if tag.Head() != "tag:v1" {
		t.Fatalf("tag Head() = %q", tag.Head())
	}
	comment := sampleContext()
	comment.Comment = &Comment{ID: 1, Author: "alice", Command: "/deploy", Arguments: "prod"}
	sched := Context{Event: EventSchedule, Branch: "main", Revision: "abcdef0123", Schedule: &Schedule{Cron: "0 3 * * *", Slot: "2026-01-01T03:00:00Z"}}
	tests := []struct {
		c    Context
		want []string
	}{
		{sampleContext(), []string{"Pull request #5", "`feature` → `main`", "`0123456`", "@alice", "approved by @bob"}},
		{comment, []string{"`/deploy` on pull request #5"}},
		{sched, []string{"Schedule `0 3 * * *`", "on `main`"}},
		{Context{Event: EventPush, Branch: "main", Revision: "abcdef0123"}, []string{"Push to `main` at `abcdef0`"}},
		{Context{Event: EventPush, Tag: "v1.0.0"}, []string{"Push of tag `v1.0.0`"}},
		{Context{Event: EventMergeGroup, MergeGroup: &MergeGroup{BaseRef: "refs/heads/main"}}, []string{"Merge queue into `main`"}},
	}
	for _, tt := range tests {
		got := tt.c.Describe()
		for _, w := range tt.want {
			if !strings.Contains(got, w) {
				t.Errorf("Describe() = %q, missing %q", got, w)
			}
		}
	}
}
