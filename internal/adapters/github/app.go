// Package github is Octomaton's code host: it authenticates as the Octomaton GitHub App, implements
// ci.CodeHost over GitHub's REST API (a report is a check run, carrying its trigger in a hidden
// marker) and decodes webhook deliveries into ci.Events. Only the owners it is configured with are
// served: their installations, repositories and events.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/system/metrics"
)

const (
	// DefaultBaseURL is the GitHub REST API endpoint.
	DefaultBaseURL = "https://api.github.com/"
	// requestTimeout bounds each attempt of a request.
	requestTimeout = 30 * time.Second
)

// App is the Octomaton GitHub App. Installation tokens are cached, and refreshed shortly before
// they expire, by ghinstallation: one transport per installation. Every request, a token's included,
// is retried per its Retries.
type App struct {
	id        int64
	baseURL   string
	transport http.RoundTripper
	owners    []string
	metrics   *metrics.Metrics
	retries   Retries
	logger    *slog.Logger
	apps      *ghinstallation.AppsTransport
	client    *github.Client

	mu            sync.Mutex
	installations map[int64]*installation
}

var _ ci.CodeHost = (*App)(nil)

// Option customizes an App.
type Option func(*App)

// WithBaseURL points the App at another GitHub API endpoint (used by tests).
func WithBaseURL(u string) Option {
	return func(a *App) { a.baseURL = u }
}

// WithTransport sets the HTTP transport of every GitHub request.
func WithTransport(rt http.RoundTripper) Option {
	return func(a *App) { a.transport = rt }
}

// WithOwners serves only the installations on, the repositories of, and the events from these
// users and organizations; without it, every owner is served.
func WithOwners(owners []string) Option {
	return func(a *App) { a.owners = owners }
}

// WithMetrics counts failed check-run calls on m.
func WithMetrics(m *metrics.Metrics) Option {
	return func(a *App) { a.metrics = m }
}

// WithRetries retries requests per r instead of DefaultRetries.
func WithRetries(r Retries) Option {
	return func(a *App) { a.retries = r }
}

// WithLogger logs requests' retries on l instead of slog's default logger.
func WithLogger(l *slog.Logger) Option {
	return func(a *App) { a.logger = l }
}

// New creates the App for an App ID and its private key.
func New(appID int64, key *rsa.PrivateKey, opts ...Option) (*App, error) {
	a := &App{
		id: appID, baseURL: DefaultBaseURL, transport: http.DefaultTransport, metrics: metrics.Discard(),
		retries: DefaultRetries, logger: slog.Default(), installations: map[int64]*installation{},
	}
	for _, opt := range opts {
		opt(a)
	}
	if !strings.HasSuffix(a.baseURL, "/") {
		a.baseURL += "/"
	}
	a.apps = ghinstallation.NewAppsTransportFromPrivateKey(a.transport, appID, key)
	a.apps.BaseURL = strings.TrimRight(a.baseURL, "/")
	client, err := a.newClient(a.apps)
	if err != nil {
		return nil, err
	}
	a.client = client
	return a, nil
}

func (a *App) newClient(rt http.RoundTripper) (*github.Client, error) {
	return github.NewClient(
		github.WithHTTPClient(&http.Client{Transport: retrying(rt, a.retries, a.logger)}),
		github.WithURLs(&a.baseURL, &a.baseURL),
		github.WithUserAgent("octomaton"),
	)
}

// ID returns the App's ID.
func (a *App) ID() int64 { return a.id }

// serves reports whether the App serves repositories owned by owner.
func (a *App) serves(owner string) bool {
	if len(a.owners) == 0 {
		return true
	}
	for _, allowed := range a.owners {
		if strings.EqualFold(allowed, owner) {
			return true
		}
	}
	return false
}

// Installation returns the (cached) API of an installation.
func (a *App) Installation(id int64) ci.Installation {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.installations[id]; ok {
		return c
	}
	client, err := a.newClient(ghinstallation.NewFromAppsTransport(a.apps, id))
	if err != nil {
		// New validated the base URL, so this cannot happen.
		panic(fmt.Sprintf("creating GitHub client: %v", err))
	}
	c := &installation{app: a, gh: client}
	a.installations[id] = c
	return c
}

// Accounts lists the served accounts the App is installed on.
func (a *App) Accounts(ctx context.Context) ([]ci.Account, error) {
	var out []ci.Account
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := a.client.Apps.ListInstallations(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing installations: %w", err)
		}
		for _, inst := range page {
			if login := inst.GetAccount().GetLogin(); a.serves(login) {
				out = append(out, ci.Account{InstallationID: inst.GetID(), Login: login})
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// RepositoryToken mints a short-lived installation token restricted to one repository. With no
// permissions it grants contents:read only.
func (a *App) RepositoryToken(ctx context.Context, installationID, repositoryID int64, permissions map[string]string) (ci.Token, error) {
	return a.installationToken(ctx, installationID, []int64{repositoryID}, permissions)
}

// InstallationToken mints a short-lived installation token for every repository the installation
// can reach. With no permissions it grants contents:read only.
func (a *App) InstallationToken(ctx context.Context, installationID int64, permissions map[string]string) (ci.Token, error) {
	return a.installationToken(ctx, installationID, nil, permissions)
}

// installationToken mints a token restricted to repositoryIDs, or for every repository of the
// installation when there are none.
func (a *App) installationToken(ctx context.Context, installationID int64, repositoryIDs []int64, permissions map[string]string) (ci.Token, error) {
	if len(permissions) == 0 {
		permissions = map[string]string{"contents": "read"}
	}
	perms, err := parsePermissions(permissions)
	if err != nil {
		return ci.Token{}, err
	}
	tok, _, err := a.client.Apps.CreateInstallationToken(ctx, installationID, &github.InstallationTokenOptions{
		RepositoryIDs: repositoryIDs,
		Permissions:   perms,
	})
	if err != nil {
		return ci.Token{}, fmt.Errorf("creating installation token: %w", err)
	}
	return ci.Token{Value: tok.GetToken(), ExpiresAt: tok.GetExpiresAt().Time, Permissions: permissions}, nil
}

// CheckPermissions reports permissions GitHub cannot grant an installation token.
func (a *App) CheckPermissions(permissions map[string]string) error {
	return CheckPermissions(permissions)
}

// CheckPermissions reports permissions GitHub cannot grant an installation token: unknown names and
// access levels other than read, write and admin.
func CheckPermissions(permissions map[string]string) error {
	_, err := parsePermissions(permissions)
	return err
}

// parsePermissions converts a permission map such as {"contents": "read"} into the API type.
// Unknown names are errors, because the API type would silently drop them.
func parsePermissions(m map[string]string) (*github.InstallationPermissions, error) {
	names := make([]string, 0, len(m))
	for name, level := range m {
		switch level {
		case "read", "write", "admin":
		default:
			return nil, fmt.Errorf("permission %q: access level must be read, write or admin (got %q)", name, level)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var perms github.InstallationPermissions
	if err := dec.Decode(&perms); err != nil {
		return nil, fmt.Errorf("invalid permissions %v: %w", names, err)
	}
	return &perms, nil
}
