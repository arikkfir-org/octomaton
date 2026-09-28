package checkrun

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	markerPrefix = "<!-- octomatron:context:"
	markerSuffix = " -->"
)

// Marker serializes c into an HTML comment that is invisible in rendered check-run
// output. Base64 keeps the payload free of "-->" and Markdown syntax.
func Marker(c Context) (string, error) {
	if c.Version == 0 {
		c.Version = ContextVersion
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("serializing trigger context: %w", err)
	}
	return markerPrefix + base64.StdEncoding.EncodeToString(data) + markerSuffix, nil
}

// MustMarker is like Marker but returns an empty string on (practically impossible) errors.
func MustMarker(c Context) string {
	m, err := Marker(c)
	if err != nil {
		return ""
	}
	return m
}

// DecodeMarker extracts the context from the last marker found in text. found is
// false when text contains no marker.
func DecodeMarker(text string) (c Context, found bool, err error) {
	start := strings.LastIndex(text, markerPrefix)
	if start < 0 {
		return Context{}, false, nil
	}
	rest := text[start+len(markerPrefix):]
	end := strings.Index(rest, markerSuffix)
	if end < 0 {
		return Context{}, true, fmt.Errorf("unterminated octomatron context marker")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest[:end]))
	if err != nil {
		return Context{}, true, fmt.Errorf("decoding octomatron context marker: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return Context{}, true, fmt.Errorf("parsing octomatron context marker: %w", err)
	}
	if c.Version != ContextVersion {
		return Context{}, true, fmt.Errorf("unsupported octomatron context version %d", c.Version)
	}
	return c, true, nil
}

// WithMarker appends the marker for c to text, keeping the result within MaxOutputLength
// by truncating text first.
func WithMarker(text string, c Context) string {
	m := MustMarker(c)
	if m == "" {
		return Truncate(text, MaxOutputLength)
	}
	sep := "\n\n"
	if text == "" {
		sep = ""
	}
	return Truncate(text, MaxOutputLength-len(m)-len(sep)) + sep + m
}
