// Package config loads and validates Octomaton's server configuration. Every setting is an
// environment variable read by envconfig: OCTOMATON_ followed by the envconfig tags of the field and
// of the structs holding it, e.g. OCTOMATON_GITHUB_APP_ID. The telemetry package reads the logging
// settings, OCTOMATON_LOG_LEVEL and OCTOMATON_LOG_FORMAT.
package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Prefix is the envconfig prefix of every variable.
const Prefix = "octomaton"

// serviceAccountNamespaceFile holds the pod's namespace when OCTOMATON_POD_NAMESPACE is unset.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Config is the server configuration.
type Config struct {
	HTTP       HTTP       `envconfig:"HTTP"`
	Webhook    Webhook    `envconfig:"WEBHOOK"`
	GitHub     GitHub     `envconfig:"GITHUB"`
	Tekton     Tekton     `envconfig:"TEKTON"`
	Namespaces Namespaces `envconfig:"NAMESPACE"`
	Relay      Relay      `envconfig:"RELAY"`
	Retention  Retention  `envconfig:"RETENTION"`
	Pod        Pod        `envconfig:"POD"`
}

// HTTP configures the server for the webhook, the probes and the metrics.
type HTTP struct {
	Address string `envconfig:"ADDRESS" default:":8080"`
}

// Webhook sizes the pool that processes accepted webhook deliveries.
type Webhook struct {
	// Workers is the number of deliveries processed at once.
	Workers int `envconfig:"WORKERS" default:"8"`
	// QueueSize is the number of accepted deliveries that may wait for a worker.
	QueueSize int `envconfig:"QUEUE_SIZE" default:"256"`
}

// GitHub holds the GitHub App's credentials and the owners it serves. The credentials are required;
// validate reports them missing, because envconfig names a missing variable without its prefix.
type GitHub struct {
	AppID AppID `envconfig:"APP_ID"`
	// PrivateKey is the App's PEM-encoded RSA private key (PKCS#1, as GitHub issues it, or PKCS#8).
	PrivateKey    string `envconfig:"PRIVATE_KEY"`
	WebhookSecret string `envconfig:"WEBHOOK_SECRET"`
	// AllowedOwners are the users and organizations whose installations are served; empty serves
	// every installation.
	AllowedOwners []string `envconfig:"ALLOWED_OWNERS"`

	key *rsa.PrivateKey
}

// AppID is a GitHub App ID. Surrounding whitespace, like the newline a secret often ends with, is
// ignored.
type AppID int64

// Decode implements envconfig.Decoder.
func (id *AppID) Decode(value string) error {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return errors.New("not a number")
	}
	*id = AppID(n)
	return nil
}

// Tekton configures links to Tekton.
type Tekton struct {
	// DashboardURL is where check runs link to their PipelineRuns.
	DashboardURL string `envconfig:"DASHBOARD_URL"`
}

// Relay forwards verified push and pull_request deliveries to other webhook receivers.
type Relay struct {
	URLs []string `envconfig:"URLS"`
}

// Retention controls what Octomaton cleans up after runs finish.
type Retention struct {
	// FreePVCsAfter is how long after a PipelineRun finished the PVCs it owns are deleted. Pods are
	// kept: the Tekton Dashboard reads logs from them.
	FreePVCsAfter time.Duration `envconfig:"FREE_PVCS_AFTER" default:"1h"`
}

// Namespaces maps repositories to the Kubernetes namespaces their PipelineRuns run in; the Tekton
// adapter validates it when it starts.
type Namespaces struct {
	// Template is a Go template over .Repository whose output is sanitized into a DNS label.
	Template string `envconfig:"TEMPLATE" default:"ci-{{ .Repository.Name }}"`
	// Overrides maps "owner/name" to a namespace, bypassing the template; the variable lists them
	// as "owner/name:namespace" pairs separated by commas.
	Overrides map[string]string `envconfig:"OVERRIDES"`
}

// Pod identifies this replica in the leader election; set it from the downward API.
type Pod struct {
	// Name defaults to the host name.
	Name string `envconfig:"NAME"`
	// Namespace holds the leader election Lease. It defaults to the service account's namespace.
	Namespace string `envconfig:"NAMESPACE"`
}

// Error lists every problem found in the configuration.
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	return "invalid configuration:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Load reads the configuration from the environment and validates it.
func Load() (*Config, error) {
	var cfg Config
	if err := envconfig.Process(Prefix, &cfg); err != nil {
		return nil, err
	}
	if problems := cfg.validate(); len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	cfg.Pod.setDefaults()
	return &cfg, nil
}

func (c *Config) validate() []string {
	var problems []string
	if c.Webhook.Workers < 1 || c.Webhook.QueueSize < 1 {
		problems = append(problems, "OCTOMATON_WEBHOOK_WORKERS and OCTOMATON_WEBHOOK_QUEUE_SIZE must be positive")
	}
	problems = append(problems, c.GitHub.validate()...)
	if u := c.Tekton.DashboardURL; u != "" {
		problems = append(problems, checkURL("OCTOMATON_TEKTON_DASHBOARD_URL", u)...)
		c.Tekton.DashboardURL = strings.TrimRight(u, "/")
	}
	for _, u := range c.Relay.URLs {
		problems = append(problems, checkURL("OCTOMATON_RELAY_URLS", u)...)
	}
	if c.Retention.FreePVCsAfter <= 0 {
		problems = append(problems, "OCTOMATON_RETENTION_FREE_PVCS_AFTER must be positive")
	}
	return problems
}

func checkURL(name, u string) []string {
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return []string{fmt.Sprintf("%s: %q is not an absolute http(s) URL", name, u)}
	}
	return nil
}

func (g *GitHub) validate() []string {
	var problems []string
	if g.AppID == 0 {
		problems = append(problems, "OCTOMATON_GITHUB_APP_ID is required")
	} else if g.AppID < 0 {
		problems = append(problems, "OCTOMATON_GITHUB_APP_ID must be a positive GitHub App ID")
	}
	if strings.TrimSpace(g.PrivateKey) == "" {
		problems = append(problems, "OCTOMATON_GITHUB_PRIVATE_KEY is required")
	} else if key, err := ParsePrivateKey([]byte(g.PrivateKey)); err != nil {
		problems = append(problems, "OCTOMATON_GITHUB_PRIVATE_KEY: "+err.Error())
	} else {
		g.key = key
	}
	if g.WebhookSecret = strings.TrimSpace(g.WebhookSecret); g.WebhookSecret == "" {
		problems = append(problems, "OCTOMATON_GITHUB_WEBHOOK_SECRET is required")
	}
	for _, owner := range g.AllowedOwners {
		if strings.TrimSpace(owner) == "" || strings.Contains(owner, "/") {
			problems = append(problems, fmt.Sprintf("OCTOMATON_GITHUB_ALLOWED_OWNERS: %q is not a GitHub user or organization name", owner))
		}
	}
	return problems
}

// Key returns the App's parsed private key.
func (g *GitHub) Key() *rsa.PrivateKey {
	return g.key
}

func (p *Pod) setDefaults() {
	if p.Name == "" {
		p.Name, _ = os.Hostname()
	}
	if p.Namespace == "" {
		if data, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
			p.Namespace = strings.TrimSpace(string(data))
		}
	}
	if p.Namespace == "" {
		p.Namespace = "octomaton"
	}
}

// ParsePrivateKey parses a PEM-encoded RSA private key (PKCS#1, as GitHub issues them, or PKCS#8).
func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM data found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not an RSA private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return key, nil
}
