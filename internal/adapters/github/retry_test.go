package github

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// reply is one answer of a test server: a status, an optional Retry-After, or a dropped connection.
type reply struct {
	status     int
	retryAfter string
	drop       bool
}

// retryLogs returns the retries logged in buf, one JSON record per line.
func retryLogs(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == "GitHub request failed; retrying" {
			out = append(out, rec)
		}
	}
	return out
}

func TestRetrying(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		replies     []reply // in order; the last one repeats
		wantStatus  int     // 0: the request fails with an error
		wantSent    int
		wantRetries int
	}{
		{name: "a blip is ridden out", method: http.MethodGet, replies: []reply{{status: 502}, {status: 504}, {status: 200}}, wantStatus: 200, wantSent: 3, wantRetries: 2},
		{name: "an outage fails after every attempt, with its last response", method: http.MethodGet, replies: []reply{{status: 503}}, wantStatus: 503, wantSent: 6, wantRetries: 5},
		{name: "a missing file is not retried", method: http.MethodGet, replies: []reply{{status: 404}}, wantStatus: 404, wantSent: 1},
		{name: "a refusal is not retried", method: http.MethodPost, replies: []reply{{status: 422}}, wantStatus: 422, wantSent: 1},
		{name: "a plain 403 is not retried", method: http.MethodGet, replies: []reply{{status: 403}}, wantStatus: 403, wantSent: 1},
		{name: "a secondary rate limit is retried when it says", method: http.MethodGet, replies: []reply{{status: 403, retryAfter: "0"}, {status: 200}}, wantStatus: 200, wantSent: 2, wantRetries: 1},
		{name: "too many requests is retried", method: http.MethodGet, replies: []reply{{status: 429, retryAfter: "0"}, {status: 200}}, wantStatus: 200, wantSent: 2, wantRetries: 1},
		{name: "a dropped connection is retried", method: http.MethodGet, replies: []reply{{drop: true}, {status: 200}}, wantStatus: 200, wantSent: 2, wantRetries: 1},
		{name: "a POST is sent again whole", method: http.MethodPost, replies: []reply{{status: 500}, {status: 201}}, wantStatus: 201, wantSent: 2, wantRetries: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				bodies []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(body))
				rep := tt.replies[min(len(bodies), len(tt.replies))-1]
				mu.Unlock()
				if rep.drop {
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				if rep.retryAfter != "" {
					w.Header().Set("Retry-After", rep.retryAfter)
				}
				w.WriteHeader(rep.status)
			}))
			defer srv.Close()
			var buf bytes.Buffer
			client := &http.Client{Transport: retrying(http.DefaultTransport, fastRetries, slog.New(slog.NewJSONHandler(&buf, nil)))}

			req, _ := http.NewRequestWithContext(context.Background(), tt.method, srv.URL+"/repos/o/r/check-runs", strings.NewReader("payload"))
			resp, err := client.Do(req)
			status := 0
			if err == nil {
				status = resp.StatusCode
				_ = resp.Body.Close()
			}
			if status != tt.wantStatus {
				t.Fatalf("status = %d (err %v), want %d", status, err, tt.wantStatus)
			}
			if len(bodies) != tt.wantSent {
				t.Fatalf("sent %d times, want %d", len(bodies), tt.wantSent)
			}
			for i, b := range bodies {
				if b != "payload" {
					t.Fatalf("attempt %d sent body %q, want the whole body", i+1, b)
				}
			}
			logs := retryLogs(t, &buf)
			if len(logs) != tt.wantRetries {
				t.Fatalf("logged %d retries, want %d: %s", len(logs), tt.wantRetries, buf.String())
			}
			for i, rec := range logs {
				if rec["attempt"] != float64(i+1) || rec["attempts"] != float64(fastRetries.Max+1) || rec["method"] != tt.method ||
					rec["path"] != "/repos/o/r/check-runs" || rec["cause"] == "" || rec["level"] != "WARN" {
					t.Fatalf("retry %d logged %v", i+1, rec)
				}
			}
		})
	}
}

func TestRetryingStopsWithItsContext(t *testing.T) {
	var sent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	client := &http.Client{Transport: retrying(http.DefaultTransport, Retries{Max: 5, WaitMin: time.Hour, WaitMax: time.Hour}, slog.New(slog.DiscardHandler))}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := client.Do(req); err == nil {
		t.Fatalf("a request whose context ends while it waits to retry must fail")
	}
	if n := sent.Load(); n != 1 {
		t.Fatalf("sent %d times, want 1: the wait must end with the context", n)
	}
}

func TestBackoff(t *testing.T) {
	respond := func(status int, retryAfter string) *http.Response {
		r := &http.Response{StatusCode: status, Header: http.Header{}}
		if retryAfter != "" {
			r.Header.Set("Retry-After", retryAfter)
		}
		return r
	}
	tests := []struct {
		name    string
		attempt int
		resp    *http.Response
		want    time.Duration
	}{
		{name: "the first wait", attempt: 0, resp: respond(502, ""), want: time.Second},
		{name: "doubles", attempt: 3, resp: respond(502, ""), want: 8 * time.Second},
		{name: "up to the longest wait", attempt: 10, resp: respond(502, ""), want: 30 * time.Second},
		{name: "after a connection error", attempt: 1, resp: nil, want: 2 * time.Second},
		{name: "as a secondary rate limit says", attempt: 0, resp: respond(403, "7"), want: 7 * time.Second},
		{name: "a secondary rate limit's wait is capped", attempt: 0, resp: respond(403, "3600"), want: maxRetryAfter},
		{name: "as too many requests says", attempt: 0, resp: respond(429, "4"), want: 4 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backoff(DefaultRetries.WaitMin, DefaultRetries.WaitMax, tt.attempt, tt.resp); got != tt.want {
				t.Fatalf("backoff = %s, want %s", got, tt.want)
			}
		})
	}
}

// failing answers the first n requests whose path contains part with a 504, and passes the rest to GitHub.
type failing struct {
	part string
	n    int
	mu   sync.Mutex
}

func (f *failing) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	fail := strings.Contains(req.URL.Path, f.part) && f.n > 0
	if fail {
		f.n--
	}
	f.mu.Unlock()
	if fail {
		return &http.Response{StatusCode: http.StatusGatewayTimeout, Status: "504 Gateway Timeout", Body: io.NopCloser(strings.NewReader("")),
			Header: http.Header{}, Request: req}, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestTheAppRetriesEveryRequest(t *testing.T) {
	tests := []struct {
		name string
		part string
	}{
		{name: "an installation's request", part: "/contents/"},
		{name: "minting the installation's token", part: "/access_tokens"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			app, srv := newApp(t, WithTransport(&failing{part: tt.part, n: 2}), WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
			srv.AddFile(fullName, "abc", ".octomaton.yaml", "hello")
			data, err := app.Installation(installationID).ReadFile(context.Background(), repo, ".octomaton.yaml", "abc")
			if err != nil || string(data) != "hello" {
				t.Fatalf("ReadFile = %q, %v; want the file after two failed attempts", data, err)
			}
			if logs := retryLogs(t, &buf); len(logs) != 2 {
				t.Fatalf("logged %d retries, want 2: %s", len(logs), buf.String())
			}
		})
	}
}
