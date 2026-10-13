package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

const testView = "projects/test-project/locations/global/buckets/_Default/views/_Default"

// fakePage is an answer of the fake Logging API: its entries, a JSON array, or a failure with
// status and body.
type fakePage struct {
	entries string
	status  int
	body    string
}

// fakeRequest is a request the fake Logging API received.
type fakeRequest struct {
	method, path string
	header       http.Header
	body         map[string]any
}

// fakeLogging is Cloud Logging's entries:list: it answers its pages in order, page i at the token
// page-i (the first without one), each naming the next.
type fakeLogging struct {
	t     *testing.T
	pages []fakePage

	mu       sync.Mutex
	requests []fakeRequest
}

// newFake starts a fake Logging API answering pages and returns a reader of the hub cluster's
// containers through it, authenticated with the token test-token.
func newFake(t *testing.T, pages ...fakePage) (*fakeLogging, *CloudLogging) {
	t.Helper()
	f := &fakeLogging{t: t, pages: pages}
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	return f, &CloudLogging{
		BaseURL: api.URL, View: testView, Project: "test-project", Cluster: "hub", Location: "me-west1-a",
		Client: NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})),
	}
}

func (f *fakeLogging) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("entries:list body: %v", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
	f.mu.Unlock()
	i := 0
	if token, _ := body["pageToken"].(string); token != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(token, "page-"))
		if err != nil || n < 1 || n >= len(f.pages) {
			f.t.Errorf("unknown page token %q", token)
			http.Error(w, "unknown page token", http.StatusBadRequest)
			return
		}
		i = n
	}
	page := f.pages[i]
	if page.status != 0 {
		w.WriteHeader(page.status)
		_, _ = w.Write([]byte(page.body))
		return
	}
	// Written as is, as Google writes it: indented, HTML unescaped.
	answer := `{"entries": ` + page.entries
	if i+1 < len(f.pages) {
		answer += fmt.Sprintf(`, "nextPageToken": "page-%d"`, i+1)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, answer+"}")
}

// received returns the requests the fake received.
func (f *fakeLogging) received() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func TestPageRequests(t *testing.T) {
	f, reader := newFake(t, fakePage{entries: `[{"textPayload": "one\n"}]`}, fakePage{entries: `[]`})
	q := Query{Namespace: "ci-octomaton", Pod: "run-pod", Container: "step-test",
		Since: time.Date(2026, 10, 12, 9, 59, 0, 0, time.UTC), Until: time.Date(2026, 10, 12, 10, 15, 0, 0, time.UTC)}
	first, err := reader.Page(context.Background(), q, "")
	if err != nil {
		t.Fatalf("first Page: %v", err)
	}
	if _, err := reader.Page(context.Background(), q, first.Next); err != nil {
		t.Fatalf("second Page: %v", err)
	}
	requests := f.received()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	tests := []struct {
		name      string
		request   fakeRequest
		wantToken any
	}{
		{name: "first page", request: requests[0], wantToken: nil},
		{name: "next page", request: requests[1], wantToken: "page-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.request
			if r.method != http.MethodPost || r.path != "/v2/entries:list" {
				t.Errorf("request = %s %s, want POST /v2/entries:list", r.method, r.path)
			}
			for header, want := range map[string]string{
				"Authorization": "Bearer test-token", "X-Goog-User-Project": "test-project", "Content-Type": "application/json",
			} {
				if got := r.header.Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
			names, _ := r.body["resourceNames"].([]any)
			if len(names) != 1 || names[0] != testView || r.body["orderBy"] != "timestamp asc" || r.body["pageSize"] != float64(1000) {
				t.Errorf("body = %v, want the view, oldest first, 1000 entries a page", r.body)
			}
			if r.body["pageToken"] != tt.wantToken {
				t.Errorf("pageToken = %v, want %v", r.body["pageToken"], tt.wantToken)
			}
		})
	}
}

func TestPageLines(t *testing.T) {
	tests := []struct {
		name    string
		entries string
		want    []string
	}{
		{name: "text loses its newline", entries: `[{"textPayload": "built\n"}]`, want: []string{"built"}},
		{name: "text loses one newline only", entries: `[{"textPayload": "built\n\n"}]`, want: []string{"built\n"}},
		{name: "text without a newline", entries: `[{"textPayload": "built"}]`, want: []string{"built"}},
		{name: "empty text is an empty line", entries: `[{"textPayload": ""}]`, want: []string{""}},
		{name: "JSON on one line", entries: "[{\"jsonPayload\": {\n  \"level\": \"info\",\n  \"msg\": \"a <b>\"\n}}]",
			want: []string{`{"level":"info","msg":"a <b>"}`}},
		{name: "neither payload", entries: `[{"protoPayload": {"@type": "x"}}, {"jsonPayload": null}]`},
		{name: "in order", entries: `[{"textPayload": "1"}, {"jsonPayload": {"n": 2}}, {"textPayload": "3\n"}]`,
			want: []string{"1", `{"n":2}`, "3"}},
		{name: "no entries", entries: `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, reader := newFake(t, fakePage{entries: tt.entries})
			page, err := reader.Page(context.Background(), Query{}, "")
			if err != nil {
				t.Fatalf("Page: %v", err)
			}
			if !slices.Equal(page.Lines, tt.want) || page.Next != "" {
				t.Fatalf("Page = %q (next %q), want %q", page.Lines, page.Next, tt.want)
			}
		})
	}
}

func TestPageErrors(t *testing.T) {
	tests := []struct {
		name string
		page fakePage
		want string
	}{
		{name: "Google's error", page: fakePage{status: http.StatusForbidden,
			body: `{"error": {"code": 403, "message": "Permission 'logging.views.access' denied", "status": "PERMISSION_DENIED"}}`},
			want: "listing log entries: 403 Forbidden: Permission 'logging.views.access' denied"},
		{name: "a proxy's page", page: fakePage{status: http.StatusBadGateway, body: "<html>Bad Gateway</html>"},
			want: "listing log entries: 502 Bad Gateway"},
		{name: "not JSON", page: fakePage{status: http.StatusOK, body: "{"}, want: "decoding log entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, reader := newFake(t, tt.page)
			_, err := reader.Page(context.Background(), Query{}, "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Page error = %v, want %q", err, tt.want)
			}
		})
	}
}
