// Package logs serves the logs of CI containers from Cloud Logging to the Tekton Dashboard, which
// asks for them when it can't read a step's log from its pod: the cluster autoscaler deletes an
// empty CI node, its pods with it, and GKE has shipped every container's output to Cloud Logging.
package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// maxLines bounds a log at 50 pages, within the 60 entries:list requests a minute Cloud Logging
	// allows a project; the response then ends with a line saying the log was cut.
	maxLines = 50 * pageSize
	// readTimeout bounds the reading of a log.
	readTimeout = 2 * time.Minute
	// startSlack and endSlack widen a TaskRun's start and completion times, its controller's clock,
	// to the timestamps of its containers' lines, their node's clock; sidecars also stop after the
	// TaskRun completes.
	startSlack = time.Minute
	endSlack   = 5 * time.Minute
	// retention is how long Cloud Logging keeps the logs (the _Default bucket's 30 days): a read
	// without a start time goes back that far.
	retention = 30 * 24 * time.Hour
)

// The last line of a log that ends early.
const (
	cutNote    = "[octomaton-logs] The log is cut here, after %d lines; Cloud Logging holds the rest."
	failedNote = "[octomaton-logs] The rest of the log could not be read from Cloud Logging."
)

// errNotServed refuses a namespace outside the prefix, which is answered as if it didn't exist.
var errNotServed = errors.New("namespace not served")

// Query selects a container's log entries whose timestamps fall between Since and Until; a zero
// Until leaves the window open.
type Query struct {
	Namespace, Pod, Container string
	Since, Until              time.Time
}

// Handler serves GET /logs/{namespace}/{pod}/{container}, as the Tekton Dashboard's external logs
// ask for it: the container's log as text, a line per entry, oldest first, sent page by page. The
// optional startTime and completionTime, the TaskRun's (RFC 3339), bound the entries read.
type Handler struct {
	reader   *CloudLogging
	prefix   string
	logger   *slog.Logger
	maxLines int
	now      func() time.Time
}

// NewHandler serves the logs that reader reads, in the namespaces whose names start with prefix.
func NewHandler(reader *CloudLogging, prefix string, logger *slog.Logger) *Handler {
	return &Handler{reader: reader, prefix: prefix, logger: logger, maxLines: maxLines, now: time.Now}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q, err := h.query(r)
	switch {
	case errors.Is(err, errNotServed):
		http.NotFound(w, r)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		h.serve(w, r, q)
	}
}

// query reads the container and the window a request names.
func (h *Handler) query(r *http.Request) (Query, error) {
	q, err := h.container(r)
	if err != nil {
		return Query{}, err
	}
	if q.Since, q.Until, err = window(r.URL.Query(), h.now()); err != nil {
		return Query{}, err
	}
	return q, nil
}

// container validates the names of the request's path: errNotServed for a namespace outside the
// prefix. Valid names are DNS labels and subdomains, which fit inside a Logging filter's quotes.
func (h *Handler) container(r *http.Request) (Query, error) {
	q := Query{Namespace: r.PathValue("namespace"), Pod: r.PathValue("pod"), Container: r.PathValue("container")}
	switch {
	case len(validation.IsDNS1123Label(q.Namespace)) > 0:
		return Query{}, fmt.Errorf("namespace %q is not a DNS label", q.Namespace)
	case len(validation.IsDNS1123Subdomain(q.Pod)) > 0:
		return Query{}, fmt.Errorf("pod %q is not a DNS subdomain", q.Pod)
	case len(validation.IsDNS1123Label(q.Container)) > 0:
		return Query{}, fmt.Errorf("container %q is not a DNS label", q.Container)
	case !strings.HasPrefix(q.Namespace, h.prefix):
		return Query{}, errNotServed
	}
	return q, nil
}

// window returns the timestamps between which to read: from a minute before startTime, or the
// retention's start without it, to five minutes after completionTime, or open without it.
func window(params url.Values, now time.Time) (since, until time.Time, err error) {
	start, err := parseTime(params, "startTime")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := parseTime(params, "completionTime")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	since = now.Add(-retention)
	if !start.IsZero() {
		since = start.Add(-startSlack)
	}
	if !end.IsZero() {
		until = end.Add(endSlack)
	}
	return since, until, nil
}

// parseTime returns the RFC 3339 time of the parameter name, zero when it is absent or empty.
func parseTime(params url.Values, name string) (time.Time, error) {
	value := params.Get(name)
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q is not an RFC 3339 time", name, value)
	}
	return t, nil
}

// serve sends q's log a page at a time, until its last page, maxLines or a failed read.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, q Query) {
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	out := &stream{w: w}
	page, err := h.reader.Page(ctx, q, "")
	left := h.maxLines
	for err == nil {
		n := min(len(page.Lines), left)
		if out.write(page.Lines[:n]) != nil {
			return // the client left
		}
		left -= n
		switch {
		case n < len(page.Lines), left == 0 && page.Next != "":
			out.note(fmt.Sprintf(cutNote, h.maxLines))
			return
		case page.Next == "":
			return
		}
		page, err = h.reader.Page(ctx, q, page.Next)
	}
	h.fail(r, out, q, err)
}

// fail ends a response whose read failed: 502 when no line was sent, else a last line saying the
// rest is missing. It logs the failure, unless the client left, which cancelled the read.
func (h *Handler) fail(r *http.Request, out *stream, q Query, err error) {
	if r.Context().Err() != nil {
		return
	}
	h.logger.ErrorContext(r.Context(), "Cannot read a log from Cloud Logging",
		"namespace", q.Namespace, "pod", q.Pod, "container", q.Container, "lines", out.lines, "error", err)
	if out.lines == 0 {
		http.Error(out.w, "Cannot read the log from Cloud Logging", http.StatusBadGateway)
		return
	}
	out.note(failedNote)
}

// stream writes a log to the client. Its first line sends the status, 200: until then, the response
// may still fail.
type stream struct {
	w     http.ResponseWriter
	lines int
}

// write sends lines, each ending in a newline, and flushes them.
func (s *stream) write(lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	for _, line := range lines {
		if _, err := io.WriteString(s.w, line+"\n"); err != nil {
			return err
		}
	}
	s.lines += len(lines)
	if err := http.NewResponseController(s.w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// note sends the log's last line. A client that left gets nothing more anyway.
func (s *stream) note(line string) {
	_ = s.write([]string{line})
}
