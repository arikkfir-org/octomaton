package pipelines

import (
	"reflect"
	"testing"

	"octomaton.dev/internal/services/ci"
)

func TestContextOf(t *testing.T) {
	repo := ci.Repository{ID: 42, Owner: "octo", Name: "repo", FullName: "octo/repo", CloneURL: "https://github.com/octo/repo.git", HTMLURL: "https://github.com/octo/repo", DefaultBranch: "main", Private: true}
	tests := []struct {
		name    string
		trigger ci.Trigger
		want    TemplateContext
	}{
		{
			name: "pull request",
			trigger: ci.Trigger{
				Event: ci.EventPullRequest, Action: "synchronize", InstallationID: 7, Repository: repo, Revision: "abc", Ref: "refs/pull/5/head",
				Branch: "feature", Sender: "alice", Pipeline: "ci", ApprovedBy: "bob", DeliveryID: "d-1",
				PullRequest: &ci.PullRequest{Number: 5, HeadRef: "feature", HeadSHA: "abc", BaseRef: "main", BaseSHA: "def", HeadRepo: "fork/repo", Author: "alice"},
			},
			want: TemplateContext{
				Event: "pull_request", Action: "synchronize", Repository: Repository{Owner: "octo", Name: "repo", FullName: "octo/repo",
					CloneURL: "https://github.com/octo/repo.git", HTMLURL: "https://github.com/octo/repo", DefaultBranch: "main", Private: true},
				Revision: "abc", Ref: "refs/pull/5/head", Branch: "feature", Sender: "alice", Pipeline: "ci",
				PullRequest: &PullRequest{Number: 5, HeadRef: "feature", HeadSHA: "abc", BaseRef: "main", BaseSHA: "def"},
			},
		},
		{
			name: "every event object",
			trigger: ci.Trigger{
				Event: ci.EventComment, Repository: repo, Tag: "v1",
				Push:       &ci.Push{Before: "a", After: "b", Created: true},
				MergeGroup: &ci.MergeGroup{HeadRef: "refs/heads/q", HeadSHA: "h", BaseRef: "refs/heads/main", BaseSHA: "b"},
				Comment:    &ci.Comment{ID: 3, Author: "alice", Command: "/deploy", Arguments: "prod"},
				Schedule:   &ci.Schedule{Cron: "0 3 * * *", Slot: "2026-01-01T03:00:00Z"},
			},
			want: TemplateContext{
				Event: "comment", Repository: RepositoryOf(repo), Tag: "v1",
				Push:       &Push{Before: "a", After: "b"},
				MergeGroup: &MergeGroup{HeadRef: "refs/heads/q", HeadSHA: "h", BaseRef: "refs/heads/main", BaseSHA: "b"},
				Comment:    &Comment{ID: 3, Author: "alice", Command: "/deploy", Arguments: "prod"},
				Schedule:   &Schedule{Cron: "0 3 * * *", Slot: "2026-01-01T03:00:00Z"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContextOf(tt.trigger); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ContextOf() = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
