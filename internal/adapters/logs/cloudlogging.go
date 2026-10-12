package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/oauth2"
)

const (
	// DefaultBaseURL is Cloud Logging's API endpoint.
	DefaultBaseURL = "https://logging.googleapis.com"
	// Scope is the OAuth scope that reads Cloud Logging.
	Scope = "https://www.googleapis.com/auth/logging.read"
	// pageSize is the most entries entries:list answers at once.
	pageSize = 1000
	// maxErrorBytes bounds the body read from a failed request.
	maxErrorBytes = 64 << 10
)

// CloudLogging reads the logs of a GKE cluster's containers from Cloud Logging, through a log view.
type CloudLogging struct {
	// BaseURL is the API's endpoint: DefaultBaseURL, but in tests.
	BaseURL string
	// View is the log view read: projects/<project>/locations/<location>/buckets/<bucket>/views/<view>.
	View string
	// Project is the quota project of the reads, which every credential but a service account's
	// needs (X-Goog-User-Project).
	Project string
	// Cluster and Location name the GKE cluster whose containers are read.
	Cluster, Location string
	// Client sends the requests, authenticated (NewClient).
	Client *http.Client
}

// NewClient returns a client that authenticates its requests with tokens, each reused until it
// expires, and traces them.
func NewClient(tokens oauth2.TokenSource) *http.Client {
	auth := &oauth2.Transport{Source: oauth2.ReuseTokenSource(nil, tokens), Base: http.DefaultTransport}
	return &http.Client{Transport: otelhttp.NewTransport(auth)}
}

// Page is a page of a container's log.
type Page struct {
	// Lines are its entries' lines, oldest first.
	Lines []string
	// Next is the token of the next page, empty after the last.
	Next string
}

// Page reads the page of q's log that token names, the first when token is empty. A page may be
// empty and still have a next one: Cloud Logging answers a long search in parts.
func (c *CloudLogging) Page(ctx context.Context, q Query, token string) (Page, error) {
	answer, err := c.list(ctx, listRequest{
		ResourceNames: []string{c.View}, Filter: c.filter(q), OrderBy: "timestamp asc", PageSize: pageSize, PageToken: token,
	})
	if err != nil {
		return Page{}, err
	}
	page := Page{Next: answer.NextPageToken}
	for _, e := range answer.Entries {
		if line, ok := e.line(); ok {
			page.Lines = append(page.Lines, line)
		}
	}
	return page, nil
}

// filter selects q's entries among the cluster's containers. Every name in it is a validated DNS
// label or subdomain, so none can leave its quotes.
func (c *CloudLogging) filter(q Query) string {
	terms := []string{
		`resource.type="k8s_container"`,
		`resource.labels.location="` + c.Location + `"`,
		`resource.labels.cluster_name="` + c.Cluster + `"`,
		`resource.labels.namespace_name="` + q.Namespace + `"`,
		`resource.labels.pod_name="` + q.Pod + `"`,
		`resource.labels.container_name="` + q.Container + `"`,
		`timestamp>="` + timestamp(q.Since) + `"`,
	}
	if !q.Until.IsZero() {
		terms = append(terms, `timestamp<="`+timestamp(q.Until)+`"`)
	}
	return strings.Join(terms, " AND ")
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// listRequest is the body of entries:list.
type listRequest struct {
	ResourceNames []string `json:"resourceNames"`
	Filter        string   `json:"filter"`
	OrderBy       string   `json:"orderBy"`
	PageSize      int      `json:"pageSize"`
	PageToken     string   `json:"pageToken,omitempty"`
}

// listResponse is the answer of entries:list.
type listResponse struct {
	Entries       []entry `json:"entries"`
	NextPageToken string  `json:"nextPageToken"`
}

func (c *CloudLogging) list(ctx context.Context, body listRequest) (*listResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v2/entries:list", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-User-Project", c.Project)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing log entries: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var answer listResponse
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, fmt.Errorf("decoding log entries: %w", err)
	}
	return &answer, nil
}

// apiError describes a failed entries:list, with the message of Google's error body when it has one.
func apiError(resp *http.Response) error {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBytes)).Decode(&body); err != nil || body.Error.Message == "" {
		return fmt.Errorf("listing log entries: %s", resp.Status)
	}
	return fmt.Errorf("listing log entries: %s: %s", resp.Status, body.Error.Message)
}

// entry holds a log entry's payloads: GKE ships a container's line as textPayload or, when the line
// is a JSON object, as jsonPayload.
type entry struct {
	TextPayload *string         `json:"textPayload"`
	JSONPayload json.RawMessage `json:"jsonPayload"`
}

// line returns the entry's line: its text without its newline, or its JSON on one line; false when
// it has neither.
func (e entry) line() (string, bool) {
	if e.TextPayload != nil {
		return strings.TrimSuffix(*e.TextPayload, "\n"), true
	}
	var compact bytes.Buffer
	if len(e.JSONPayload) == 0 || json.Compact(&compact, e.JSONPayload) != nil || compact.String() == "null" {
		return "", false
	}
	return compact.String(), true
}
