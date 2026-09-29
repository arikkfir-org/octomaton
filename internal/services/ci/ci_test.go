package ci

import (
	"encoding/json"
	"strings"
	"testing"
)

func pullRequestTrigger() Trigger {
	return Trigger{
		Version:        TriggerVersion,
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

// TestTriggerJSON pins the serialized trigger: runs and reports created by earlier versions store
// it, and re-running them must read it back.
func TestTriggerJSON(t *testing.T) {
	tests := []struct {
		name    string
		trigger Trigger
		want    string
	}{
		{
			name:    "pull request",
			trigger: pullRequestTrigger(),
			want: `{"v":1,"event":"pull_request","action":"synchronize","deliveryID":"d-1","installationID":7,` +
				`"repository":{"id":42,"owner":"octo","name":"repo","fullName":"octo/repo","cloneURL":"https://github.com/octo/repo.git","defaultBranch":"main"},` +
				`"revision":"0123456789abcdef0123456789abcdef01234567","ref":"refs/pull/5/head","branch":"feature","sender":"alice","pipeline":"ci",` +
				`"pullRequest":{"number":5,"headRef":"feature","headSHA":"0123456789abcdef0123456789abcdef01234567","baseRef":"main","baseSHA":"abc","headRepo":"fork/repo","authorAssociation":"NONE"},` +
				`"approvedBy":"bob"}`,
		},
		{
			name: "every event object",
			trigger: Trigger{
				Version: TriggerVersion, Event: EventComment, InstallationID: 1, Repository: Repository{ID: 2, Owner: "o", Name: "r", FullName: "o/r", HTMLURL: "https://github.com/o/r", Private: true},
				Revision: "abc", Tag: "v1", Push: &Push{Before: "a", After: "b", Created: true},
				MergeGroup: &MergeGroup{HeadRef: "refs/heads/q", HeadSHA: "h", BaseRef: "refs/heads/main", BaseSHA: "b"},
				Comment:    &Comment{ID: 3, Author: "alice", Command: "/deploy", Arguments: "prod"},
				Schedule:   &Schedule{Cron: "0 3 * * *", Slot: "2026-01-01T03:00:00Z"},
				ConfigRef:  "main", RerunBy: "carol",
			},
			want: `{"v":1,"event":"comment","installationID":1,"repository":{"id":2,"owner":"o","name":"r","fullName":"o/r","htmlURL":"https://github.com/o/r","private":true},` +
				`"revision":"abc","tag":"v1","push":{"before":"a","after":"b","created":true},` +
				`"mergeGroup":{"headRef":"refs/heads/q","headSHA":"h","baseRef":"refs/heads/main","baseSHA":"b"},` +
				`"comment":{"id":3,"author":"alice","command":"/deploy","arguments":"prod"},"schedule":{"cron":"0 3 * * *","slot":"2026-01-01T03:00:00Z"},` +
				`"configRef":"main","rerunBy":"carol"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.trigger)
			if err != nil || string(data) != tt.want {
				t.Fatalf("json.Marshal = %s, %v\nwant %s", data, err, tt.want)
			}
		})
	}
}

func TestTriggerRefs(t *testing.T) {
	tests := []struct {
		name               string
		trigger            Trigger
		configAt, wantHead string
	}{
		{"pull request", pullRequestTrigger(), "0123456789abcdef0123456789abcdef01234567", "feature"},
		{"comment reads the default branch", Trigger{Revision: "abc", ConfigRef: "main", Branch: "feature"}, "main", "feature"},
		{"tag", Trigger{Revision: "abc", Tag: "v1"}, "abc", "tag:v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.trigger.ConfigAt(); got != tt.configAt {
				t.Errorf("ConfigAt() = %q, want %q", got, tt.configAt)
			}
			if got := tt.trigger.Head(); got != tt.wantHead {
				t.Errorf("Head() = %q, want %q", got, tt.wantHead)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	comment := pullRequestTrigger()
	comment.Comment = &Comment{ID: 1, Author: "alice", Command: "/deploy", Arguments: "prod"}
	rerun := pullRequestTrigger()
	rerun.RerunBy = "carol"
	tests := []struct {
		name    string
		trigger Trigger
		want    string
	}{
		{"pull request", pullRequestTrigger(), "Pull request #5 (`feature` → `main`), synchronize at `0123456` by @alice; approved by @bob"},
		{"re-run", rerun, "Pull request #5 (`feature` → `main`), synchronize at `0123456` by @alice; approved by @bob; re-run by @carol"},
		{"comment", comment, "`/deploy` on pull request #5 at `0123456` by @alice; approved by @bob"},
		{"schedule", Trigger{Event: EventSchedule, Branch: "main", Revision: "abcdef0123", Schedule: &Schedule{Cron: "0 3 * * *", Slot: "2026-01-01T03:00:00Z"}},
			"Schedule `0 3 * * *` (slot 2026-01-01T03:00:00Z) on `main` at `abcdef0`"},
		{"push", Trigger{Event: EventPush, Branch: "main", Revision: "abcdef0123"}, "Push to `main` at `abcdef0`"},
		{"tag", Trigger{Event: EventPush, Tag: "v1.0.0"}, "Push of tag `v1.0.0`"},
		{"merge group", Trigger{Event: EventMergeGroup, MergeGroup: &MergeGroup{BaseRef: "refs/heads/main"}}, "Merge queue into `main`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.trigger.Describe(); got != tt.want {
				t.Fatalf("Describe() = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestShortSHA(t *testing.T) {
	for in, want := range map[string]string{"0123456789": "0123456", "abc": "abc", "": ""} {
		if got := ShortSHA(in); got != want {
			t.Errorf("ShortSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEventString(t *testing.T) {
	repo := Repository{FullName: "octo/repo"}
	tests := []struct {
		event Event
		want  string
	}{
		{&TriggerEvent{Trigger: Trigger{Event: EventPush, Repository: repo, Revision: "0123456789"}}, "push octo/repo@0123456"},
		{&MergeGroupDestroyed{Trigger: Trigger{Repository: repo}}, "merge_group destroyed octo/repo"},
		{&CommandEvent{Repository: repo}, "comment octo/repo"},
		{&RerunEvent{Repository: repo}, "re-run octo/repo"},
	}
	for _, tt := range tests {
		if got := tt.event.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func TestSmallHelpers(t *testing.T) {
	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"admin can write", Permission("admin").CanWrite(), true},
		{"maintain can write", Permission("maintain").CanWrite(), true},
		{"write can write", Permission("write").CanWrite(), true},
		{"triage cannot write", Permission("triage").CanWrite(), false},
		{"none cannot write", Permission("none").CanWrite(), false},
		{"pending is not finished", TaskPending.Finished(), false},
		{"running is not finished", TaskRunning.Finished(), false},
		{"skipped is finished", TaskSkipped.Finished(), true},
		{"a newer run supersedes", Cancellation{SupersededBy: "run-2"}.Superseded(), true},
		{"a newer commit supersedes", Cancellation{NewerCommit: "abc"}.Superseded(), true},
		{"a reason alone does not", Cancellation{Reason: "merge group destroyed"}.Superseded(), false},
		{"refusals read as errors", strings.Contains((&Refusal{Title: "Refused", Reason: "no"}).Error(), "Refused: no"), true},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if (RunID{Tenant: "ci-repo", Name: "run-1"}).String() != "ci-repo/run-1" {
		t.Errorf("RunID.String() does not join tenant and name")
	}
}
