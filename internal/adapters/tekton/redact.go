package tekton

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// redacted replaces every secret in what Octomaton passes on from a run (log tails, results,
// failure messages) on its way to the code host, where a public repository shows it to anyone.
const redacted = "[REDACTED]"

// minSecretLength is the shortest Secret value redacted by value: shorter ones would match
// ordinary words and numbers. Values this short are left to the credential patterns.
const minSecretLength = 8

// credentials match well-known credential formats, whatever Secret (or none) they came from: a run
// can also hold credentials Octomaton never sees, such as ones it fetches with its own identity.
// Each match is replaced whole, except groups named keep and tail, which stay around the redaction.
var credentials = []*regexp.Regexp{
	// GitHub tokens: personal, OAuth, user-to-server, installation, refresh and fine-grained.
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})`),
	// PEM private keys, also when the tail cut off their first or last line.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`),
	regexp.MustCompile(`(?s)\A.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	// Google API keys, OAuth access tokens, OAuth client secrets and refresh tokens.
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`),
	regexp.MustCompile(`\bya29\.[0-9A-Za-z_\-]{20,}`),
	regexp.MustCompile(`\bGOCSPX-[0-9A-Za-z_\-]{20,}`),
	regexp.MustCompile(`\b1//0[0-9A-Za-z_\-]{30,}`),
	// "sk-" API keys (OpenAI, Anthropic, DeepSeek and others).
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}`),
	// JSON Web Tokens.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
	// AWS access key IDs, Slack tokens.
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9\-]{10,}`),
	// Credentials in an Authorization header and in a URL's user information.
	regexp.MustCompile(`(?i)(?P<keep>authorization:\s*(?:bearer|basic|token)\s+)[^\s"']+`),
	regexp.MustCompile(`(?P<keep>\b[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s/:@]+:)[^\s/@]+(?P<tail>@)`),
}

// encoded matches a run of characters that could be base64 (standard or URL-safe) long enough to
// hide a secret.
var encoded = regexp.MustCompile(`[A-Za-z0-9+/_\-]{16,}={0,2}`)

// redactor removes a run's secrets from text.
type redactor struct {
	// values are the Secret values to remove, longest first so a value inside another can't leave
	// part of the longer one behind.
	values []string
}

// newRedactor returns a redactor for the given Secret values. A multi-line value (a private key)
// is also removed line by line, since a log tail may hold only some of its lines.
func newRedactor(values [][]byte) *redactor {
	r := &redactor{}
	add := func(v string) {
		if v = strings.TrimSpace(v); len(v) >= minSecretLength && !slices.Contains(r.values, v) {
			r.values = append(r.values, v)
		}
	}
	for _, v := range values {
		add(string(v))
		for line := range strings.SplitSeq(string(v), "\n") {
			add(line)
		}
	}
	slices.SortFunc(r.values, func(a, b string) int { return len(b) - len(a) })
	return r
}

// Redact removes from s every Secret value, every base64 encoding that holds one or a known
// credential, and every known credential.
func (r *redactor) Redact(s string) string {
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, redacted)
	}
	s = encoded.ReplaceAllStringFunc(s, func(candidate string) string {
		if decoded, ok := decodeBase64(candidate); ok && r.holdsSecret(decoded) {
			return redacted
		}
		return candidate
	})
	return redactCredentials(s)
}

func (r *redactor) holdsSecret(s string) bool {
	for _, v := range r.values {
		if strings.Contains(s, v) {
			return true
		}
	}
	return redactCredentials(s) != s
}

// redactCredentials removes every match of the known credential formats.
func redactCredentials(s string) string {
	for _, re := range credentials {
		keep, tail := re.SubexpIndex("keep"), re.SubexpIndex("tail")
		s = re.ReplaceAllStringFunc(s, func(match string) string {
			groups := re.FindStringSubmatch(match)
			out := redacted
			if keep >= 0 {
				out = groups[keep] + out
			}
			if tail >= 0 {
				out += groups[tail]
			}
			return out
		})
	}
	return s
}

// decodeBase64 decodes s in any of base64's four alphabets and paddings.
func decodeBase64(s string) (string, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
	}
	return "", false
}

// redactorFor returns the redactor of a run: the values of every Secret its PipelineRun references,
// its GitHub token Secret included. A Secret that no longer exists has nothing to leak; any other
// failure to read one is an error, so the caller withholds what it can't redact.
func (c *kubeClient) redactorFor(ctx context.Context, pr *unstructured.Unstructured) (*redactor, error) {
	var values [][]byte
	for _, name := range slices.Compact(secrets(pr.Object)) {
		s, err := c.Kube.CoreV1().Secrets(pr.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading Secret %s/%s to redact the run's output: %w", pr.GetNamespace(), name, err)
		}
		for _, v := range s.Data {
			values = append(values, v)
		}
		for _, v := range s.StringData {
			values = append(values, []byte(v))
		}
	}
	return newRedactor(values), nil
}
