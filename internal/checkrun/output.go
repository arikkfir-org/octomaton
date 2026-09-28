package checkrun

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// MaxOutputLength is GitHub's limit for a check run's output summary and text.
const MaxOutputLength = 65535

// MaxSummaryLength caps the summaries Switchboard writes, below GitHub's limit.
const MaxSummaryLength = 60000

const truncationNote = "\n\n_(truncated)_"

// Truncate shortens s to at most limit bytes (never splitting a UTF-8 sequence),
// ending it with a note when anything was cut.
func Truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= len(truncationNote) {
		return cutRunes(s, limit)
	}
	return cutRunes(s, limit-len(truncationNote)) + truncationNote
}

// TruncateHead keeps the last bytes of s (at most limit), never splitting a UTF-8 sequence.
func TruncateHead(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	start := len(s) - limit
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func cutRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// DashboardURL links to a PipelineRun in the Tekton Dashboard; empty when no
// dashboard is configured.
func DashboardURL(base, namespace, name string) string {
	return dashboardLink(base, namespace, "pipelineruns", name)
}

// TaskRunURL links to a TaskRun in the Tekton Dashboard; empty when no dashboard is configured.
func TaskRunURL(base, namespace, name string) string {
	return dashboardLink(base, namespace, "taskruns", name)
}

func dashboardLink(base, namespace, kind, name string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/#/namespaces/" + url.PathEscape(namespace) + "/" + kind + "/" + url.PathEscape(name)
}
