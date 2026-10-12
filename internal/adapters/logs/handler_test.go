package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testNow is the time of every test request.
var testNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// testHandler serves the logs of the fake's pages in namespaces starting with ci-, up to maxLines
// (0: the default), logging to logs.
func testHandler(t *testing.T, maxLines int, logs io.Writer, pages ...fakePage) (*fakeLogging, http.Handler) {
	t.Helper()
	f, reader := newFake(t, pages...)
	h := NewHandler(reader, "ci-", slog.New(slog.NewJSONHandler(logs, nil)))
	h.now = func() time.Time { return testNow }
	if maxLines > 0 {
		h.maxLines = maxLines
	}
	mux := http.NewServeMux()
	mux.Handle(route, h)
	return f, mux
}

// get serves a GET of target on ctx.
func get(ctx context.Context, h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil))
	return rec
}

// errorLogs returns the error records written to logs.
func errorLogs(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for dec := json.NewDecoder(logs); dec.More(); {
		var record map[string]any
		if err := dec.Decode(&record); err != nil {
			t.Fatalf("decoding the logs: %v", err)
		}
		if record["level"] == "ERROR" {
			records = append(records, record)
		}
	}
	return records
}

func TestHandlerValidatesTheRequest(t *testing.T) {
	tests := []struct {
		name, target string
		wantCode     int
	}{
		{name: "a CI container", target: "/logs/ci-octomaton/run-pod/step-test", wantCode: http.StatusOK},
		{name: "a pod name with dots", target: "/logs/ci-octomaton/run.1-pod/step-test", wantCode: http.StatusOK},
		{name: "both times", target: "/logs/ci-a/p/c?startTime=2026-10-12T10:00:00Z&completionTime=2026-10-12T10:10:00Z", wantCode: http.StatusOK},
		{name: "a namespace outside the prefix", target: "/logs/kube-system/p/c", wantCode: http.StatusNotFound},
		{name: "the prefix inside the namespace", target: "/logs/x-ci-a/p/c", wantCode: http.StatusNotFound},
		{name: "an upper-case namespace", target: "/logs/CI-octomaton/p/c", wantCode: http.StatusBadRequest},
		{name: "a namespace over 63 characters", target: "/logs/ci-" + strings.Repeat("a", 61) + "/p/c", wantCode: http.StatusBadRequest},
		{name: "a pod with an underscore", target: "/logs/ci-a/run_pod/c", wantCode: http.StatusBadRequest},
		{name: "a pod with an escaped slash", target: "/logs/ci-a/run%2Fpod/c", wantCode: http.StatusBadRequest},
		{name: "a pod with an escaped quote", target: "/logs/ci-a/run%22pod/c", wantCode: http.StatusBadRequest},
		{name: "a pod over 253 characters", target: "/logs/ci-a/" + strings.Repeat("a", 254) + "/c", wantCode: http.StatusBadRequest},
		{name: "a container with a dot", target: "/logs/ci-a/p/step.test", wantCode: http.StatusBadRequest},
		{name: "a container over 63 characters", target: "/logs/ci-a/p/" + strings.Repeat("a", 64), wantCode: http.StatusBadRequest},
		{name: "a start time that isn't RFC 3339", target: "/logs/ci-a/p/c?startTime=yesterday", wantCode: http.StatusBadRequest},
		{name: "a completion time without its zone", target: "/logs/ci-a/p/c?completionTime=2026-10-12T10:00:00", wantCode: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, h := testHandler(t, 0, io.Discard, fakePage{entries: `[]`})
			rec := get(context.Background(), h, tt.target)
			if rec.Code != tt.wantCode {
				t.Fatalf("status %d (%q), want %d", rec.Code, rec.Body.String(), tt.wantCode)
			}
			wantReads := 0
			if tt.wantCode == http.StatusOK {
				wantReads = 1
			}
			if got := len(f.received()); got != wantReads {
				t.Fatalf("Cloud Logging read %d times, want %d", got, wantReads)
			}
		})
	}
}

func TestHandlerFiltersTheContainerAndItsWindow(t *testing.T) {
	container := `resource.type="k8s_container" AND resource.labels.location="me-west1-a" AND resource.labels.cluster_name="hub"` +
		` AND resource.labels.namespace_name="ci-octomaton" AND resource.labels.pod_name="octomaton-ci-abc1234-1-ci-pod"` +
		` AND resource.labels.container_name="step-test"`
	tests := []struct {
		name, query, want string
	}{
		{name: "a finished TaskRun", query: "?startTime=2026-10-12T10:00:00Z&completionTime=2026-10-12T10:10:00Z",
			want: container + ` AND timestamp>="2026-10-12T09:59:00Z" AND timestamp<="2026-10-12T10:15:00Z"`},
		{name: "a running TaskRun", query: "?startTime=2026-10-12T10:00:00Z",
			want: container + ` AND timestamp>="2026-10-12T09:59:00Z"`},
		{name: "no times: the retention", want: container + ` AND timestamp>="2026-09-12T12:00:00Z"`},
		{name: "empty times", query: "?startTime=&completionTime=", want: container + ` AND timestamp>="2026-09-12T12:00:00Z"`},
		{name: "an offset and fractions", query: "?startTime=2026-10-12T13:00:00.25%2B03:00&completionTime=2026-10-12T10:10:00.5Z",
			want: container + ` AND timestamp>="2026-10-12T09:59:00.25Z" AND timestamp<="2026-10-12T10:15:00.5Z"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, h := testHandler(t, 0, io.Discard, fakePage{entries: `[]`})
			if rec := get(context.Background(), h, "/logs/ci-octomaton/octomaton-ci-abc1234-1-ci-pod/step-test"+tt.query); rec.Code != http.StatusOK {
				t.Fatalf("status %d (%q)", rec.Code, rec.Body.String())
			}
			requests := f.received()
			if len(requests) != 1 || requests[0].body["filter"] != tt.want {
				t.Fatalf("filters = %v,\nwant %s", requests, tt.want)
			}
		})
	}
}

func TestHandlerStreamsTheLog(t *testing.T) {
	ab := `[{"textPayload": "a\n"}, {"textPayload": "b\n"}]`
	tests := []struct {
		name       string
		maxLines   int
		pages      []fakePage
		wantCode   int
		wantBody   string
		wantReads  int
		wantErrors int
	}{
		{name: "two pages", pages: []fakePage{{entries: ab}, {entries: `[{"jsonPayload": {"c": true}}]`}},
			wantCode: http.StatusOK, wantBody: "a\nb\n{\"c\":true}\n", wantReads: 2},
		{name: "no entries", pages: []fakePage{{entries: `[]`}}, wantCode: http.StatusOK, wantReads: 1},
		{name: "an empty page before the entries", pages: []fakePage{{entries: `[]`}, {entries: ab}},
			wantCode: http.StatusOK, wantBody: "a\nb\n", wantReads: 2},
		{name: "cut inside a page", maxLines: 3, pages: []fakePage{{entries: ab}, {entries: ab}},
			wantCode: http.StatusOK, wantBody: "a\nb\na\n" + "[octomaton-logs] The log is cut here, after 3 lines; Cloud Logging holds the rest.\n", wantReads: 2},
		{name: "cut at a page's end", maxLines: 2, pages: []fakePage{{entries: ab}, {entries: ab}},
			wantCode: http.StatusOK, wantBody: "a\nb\n" + "[octomaton-logs] The log is cut here, after 2 lines; Cloud Logging holds the rest.\n", wantReads: 1},
		{name: "whole at the limit", maxLines: 2, pages: []fakePage{{entries: ab}},
			wantCode: http.StatusOK, wantBody: "a\nb\n", wantReads: 1},
		{name: "the first read fails", pages: []fakePage{{status: http.StatusTooManyRequests}},
			wantCode: http.StatusBadGateway, wantBody: "Cannot read the log from Cloud Logging\n", wantReads: 1, wantErrors: 1},
		{name: "a later read fails", pages: []fakePage{{entries: ab}, {status: http.StatusServiceUnavailable}},
			wantCode: http.StatusOK, wantBody: "a\nb\n" + failedNote + "\n", wantReads: 2, wantErrors: 1},
		{name: "a read fails after empty pages", pages: []fakePage{{entries: `[]`}, {status: http.StatusInternalServerError}},
			wantCode: http.StatusBadGateway, wantBody: "Cannot read the log from Cloud Logging\n", wantReads: 2, wantErrors: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			f, h := testHandler(t, tt.maxLines, &logs, tt.pages...)
			rec := get(context.Background(), h, "/logs/ci-octomaton/run-pod/step-test?startTime=2026-10-12T10:00:00Z")
			if rec.Code != tt.wantCode || rec.Body.String() != tt.wantBody {
				t.Fatalf("%d %q, want %d %q", rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q, want text/plain", got)
			}
			if got := len(f.received()); got != tt.wantReads {
				t.Errorf("Cloud Logging read %d times, want %d", got, tt.wantReads)
			}
			if got := len(errorLogs(t, &logs)); got != tt.wantErrors {
				t.Errorf("%d errors logged, want %d", got, tt.wantErrors)
			}
		})
	}
}

func TestHandlerLogsFailedReadsOnce(t *testing.T) {
	denied := fakePage{status: http.StatusForbidden, body: `{"error": {"code": 403, "message": "Permission denied"}}`}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name      string
		ctx       context.Context
		wantError map[string]any // nil: nothing logged
	}{
		{name: "Cloud Logging refuses", ctx: context.Background(), wantError: map[string]any{
			"msg": "Cannot read a log from Cloud Logging", "namespace": "ci-octomaton", "pod": "run-pod", "container": "step-test",
			"lines": float64(0), "error": "listing log entries: 403 Forbidden: Permission denied",
		}},
		{name: "the client left", ctx: cancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			_, h := testHandler(t, 0, &logs, denied)
			get(tt.ctx, h, "/logs/ci-octomaton/run-pod/step-test")
			records := errorLogs(t, &logs)
			if tt.wantError == nil {
				if len(records) != 0 {
					t.Fatalf("logged %v, want nothing", records)
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("logged %v, want one error", records)
			}
			for key, want := range tt.wantError {
				if got := records[0][key]; got != want {
					t.Errorf("%s = %v, want %v", key, got, want)
				}
			}
		})
	}
}
