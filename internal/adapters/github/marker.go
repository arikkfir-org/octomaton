package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"octomaton.dev/internal/services/ci"
)

const (
	markerPrefix = "<!-- octomaton:context:"
	markerSuffix = " -->"
)

// MaxOutputLength is GitHub's limit for a check run's output summary and text.
const MaxOutputLength = 65535

// MaxSummaryLength caps the summaries and comments Octomaton writes, below GitHub's limit.
const MaxSummaryLength = 60000

const truncationNote = "\n\n_(truncated)_"

// Marker serializes a trigger into an HTML comment, invisible in rendered check-run output. Base64
// keeps the payload free of "-->" and Markdown syntax.
func Marker(t ci.Trigger) (string, error) {
	if t.Version == 0 {
		t.Version = ci.TriggerVersion
	}
	data, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("serializing the trigger: %w", err)
	}
	return markerPrefix + base64.StdEncoding.EncodeToString(data) + markerSuffix, nil
}

// DecodeMarker extracts the trigger from the last marker in text. found is false when text holds
// no marker.
func DecodeMarker(text string) (t ci.Trigger, found bool, err error) {
	start := strings.LastIndex(text, markerPrefix)
	if start < 0 {
		return ci.Trigger{}, false, nil
	}
	rest := text[start+len(markerPrefix):]
	end := strings.Index(rest, markerSuffix)
	if end < 0 {
		return ci.Trigger{}, true, fmt.Errorf("unterminated octomaton context marker")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest[:end]))
	if err != nil {
		return ci.Trigger{}, true, fmt.Errorf("decoding octomaton context marker: %w", err)
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return ci.Trigger{}, true, fmt.Errorf("parsing octomaton context marker: %w", err)
	}
	if t.Version != ci.TriggerVersion {
		return ci.Trigger{}, true, fmt.Errorf("unsupported octomaton context version %d", t.Version)
	}
	return t, true, nil
}

// WithMarker appends the trigger's marker to text, truncating text first so the result stays
// within MaxOutputLength.
func WithMarker(text string, t ci.Trigger) string {
	m, err := Marker(t)
	if err != nil {
		return Truncate(text, MaxOutputLength)
	}
	sep := "\n\n"
	if text == "" {
		sep = ""
	}
	return Truncate(text, MaxOutputLength-len(m)-len(sep)) + sep + m
}

// Truncate shortens s to at most limit bytes, never splitting a UTF-8 sequence, and ends it with a
// note when anything was cut.
func Truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= len(truncationNote) {
		return cutRunes(s, limit)
	}
	return cutRunes(s, limit-len(truncationNote)) + truncationNote
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
