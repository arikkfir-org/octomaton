package config

import (
	"fmt"
	"regexp"

	"github.com/kelseyhightower/envconfig"
)

// Logs is the configuration of octomaton-logs, which serves the logs of CI containers from Cloud
// Logging. Every setting is OCTOMATON_LOGS_ followed by the envconfig tag of its field, e.g.
// OCTOMATON_LOGS_VIEW.
type Logs struct {
	// Address serves /logs, /healthz and /readyz.
	Address string `envconfig:"ADDRESS" default:":8080"`
	// View is the log view read: projects/<project>/locations/<location>/buckets/<bucket>/views/<view>.
	View string `envconfig:"VIEW"`
	// Cluster and Location name the GKE cluster whose containers' logs are served.
	Cluster  string `envconfig:"CLUSTER"`
	Location string `envconfig:"LOCATION"`
	// NamespacePrefix starts the name of every namespace served; it keeps the server to CI tenants.
	NamespacePrefix string `envconfig:"NAMESPACE_PREFIX" default:"ci-"`

	project string
}

var (
	// logViewRE matches a log view's name; its group is the project.
	logViewRE = regexp.MustCompile(`^projects/([a-z0-9][-a-z0-9.:]*[a-z0-9])/locations/[-a-z0-9]+/buckets/[-_.A-Za-z0-9]+/views/[-_.A-Za-z0-9]+$`)
	// gkeNameRE matches GKE's cluster and location names, which go inside a Logging filter's quotes.
	gkeNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	// namespacePrefixRE matches the start of a namespace name.
	namespacePrefixRE = regexp.MustCompile(`^[a-z0-9][-a-z0-9]{0,62}$`)
)

// LoadLogs reads octomaton-logs' configuration from the environment and validates it.
func LoadLogs() (*Logs, error) {
	var cfg Logs
	if err := envconfig.Process(Prefix+"_logs", &cfg); err != nil {
		return nil, err
	}
	if problems := cfg.validate(); len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return &cfg, nil
}

// Project returns the view's project, the quota project of the reads.
func (l *Logs) Project() string {
	return l.project
}

func (l *Logs) validate() []string {
	var problems []string
	if l.View == "" {
		problems = append(problems, "OCTOMATON_LOGS_VIEW is required")
	} else if m := logViewRE.FindStringSubmatch(l.View); m == nil {
		problems = append(problems, fmt.Sprintf("OCTOMATON_LOGS_VIEW: %q is not a log view, projects/<project>/locations/<location>/buckets/<bucket>/views/<view>", l.View))
	} else {
		l.project = m[1]
	}
	problems = append(problems, checkGKEName("OCTOMATON_LOGS_CLUSTER", "GKE cluster name", l.Cluster)...)
	problems = append(problems, checkGKEName("OCTOMATON_LOGS_LOCATION", "GKE location", l.Location)...)
	if l.NamespacePrefix == "" {
		problems = append(problems, "OCTOMATON_LOGS_NAMESPACE_PREFIX must not be empty: it keeps the server to CI namespaces")
	} else if !namespacePrefixRE.MatchString(l.NamespacePrefix) {
		problems = append(problems, fmt.Sprintf("OCTOMATON_LOGS_NAMESPACE_PREFIX: %q does not start a namespace name", l.NamespacePrefix))
	}
	return problems
}

func checkGKEName(variable, what, value string) []string {
	switch {
	case value == "":
		return []string{variable + " is required"}
	case !gkeNameRE.MatchString(value):
		return []string{fmt.Sprintf("%s: %q is not a %s", variable, value, what)}
	}
	return nil
}
