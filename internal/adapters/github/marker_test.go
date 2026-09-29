package github

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"octomaton.dev/internal/services/ci"
)

func sampleTrigger() ci.Trigger {
	return ci.Trigger{
		Version:        ci.TriggerVersion,
		Event:          ci.EventPullRequest,
		Action:         "synchronize",
		DeliveryID:     "d-1",
		InstallationID: 7,
		Repository:     ci.Repository{ID: 42, Owner: "octo", Name: "repo", FullName: "octo/repo", CloneURL: "https://github.com/octo/repo.git", DefaultBranch: "main"},
		Revision:       "0123456789abcdef0123456789abcdef01234567",
		Ref:            "refs/pull/5/head",
		Branch:         "feature",
		Sender:         "alice",
		Pipeline:       "ci",
		PullRequest:    &ci.PullRequest{Number: 5, HeadRef: "feature", HeadSHA: "0123456789abcdef0123456789abcdef01234567", BaseRef: "main", BaseSHA: "abc", HeadRepo: "fork/repo", AuthorAssociation: "NONE"},
		ApprovedBy:     "bob",
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	want := sampleTrigger()
	m, err := Marker(want)
	if err != nil {
		t.Fatalf("Marker: %v", err)
	}
	if !strings.HasPrefix(m, "<!-- octomaton:context:") || !strings.HasSuffix(m, " -->") || strings.Count(m, "-->") != 1 {
		t.Fatalf("marker is not a single HTML comment: %q", m)
	}
	got, found, err := DecodeMarker("Some text\n\n" + m)
	if err != nil || !found {
		t.Fatalf("DecodeMarker: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestMarkerSetsVersion(t *testing.T) {
	trigger := sampleTrigger()
	trigger.Version = 0
	m, _ := Marker(trigger)
	got, found, err := DecodeMarker(m)
	if err != nil || !found || got.Version != ci.TriggerVersion {
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
		{name: "unterminated", text: "<!-- octomaton:context:abc", wantFound: true, wantErr: "unterminated"},
		{name: "bad base64", text: "<!-- octomaton:context:!!! -->", wantFound: true, wantErr: "decoding"},
		{name: "bad JSON", text: "<!-- octomaton:context:" + enc("nope") + " -->", wantFound: true, wantErr: "parsing"},
		{name: "unknown version", text: "<!-- octomaton:context:" + enc(`{"v":99}`) + " -->", wantFound: true, wantErr: "unsupported"},
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
	first, second := sampleTrigger(), sampleTrigger()
	second.Pipeline = "release"
	m1, _ := Marker(first)
	m2, _ := Marker(second)
	got, _, err := DecodeMarker(m1 + "\n" + m2)
	if err != nil || got.Pipeline != "release" {
		t.Fatalf("got pipeline %q, %v", got.Pipeline, err)
	}
}

func TestWithMarkerKeepsTheMarkerWithinTheLimit(t *testing.T) {
	trigger := sampleTrigger()
	text := WithMarker(strings.Repeat("log line ✓\n", 20000), trigger)
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
	if m, _ := Marker(trigger); WithMarker("", trigger) != m {
		t.Fatalf("an empty text is just the marker")
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		check func(string) bool
	}{
		{"short text stays", "short", 10, func(s string) bool { return s == "short" }},
		{"long text ends with a note", strings.Repeat("é", 100), 50, func(s string) bool {
			return len(s) <= 50 && utf8.ValidString(s) && strings.HasSuffix(s, "_(truncated)_")
		}},
		{"a limit below the note cuts runes only", strings.Repeat("é", 10), 5, func(s string) bool { return s == "éé" }},
	}
	for _, tt := range tests {
		if got := Truncate(tt.in, tt.limit); !tt.check(got) {
			t.Errorf("%s: Truncate = %q (%d bytes)", tt.name, got, len(got))
		}
	}
}
