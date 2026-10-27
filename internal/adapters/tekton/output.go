package tekton

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"octomaton.dev/internal/services/ci"
)

// maxOutputLine bounds an output line: a check's summary holds at most 64 KiB, which base64 turns
// into about 86 KiB. A longer line is skipped.
const maxOutputLine = 128 << 10

// scanOutput reads the output lines of logs ("octomaton:<name> <base64>") into values, the newest
// of each of names winning. A line that is too long, isn't one of names, or doesn't decode to UTF-8
// text, and a last line without its newline (the runtime may still be writing it), are skipped.
func scanOutput(logs io.Reader, names []string, values map[string]string) error {
	r := bufio.NewReader(logs)
	var line []byte
	long := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !long {
			if len(line)+len(chunk) > maxOutputLine {
				long, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
		if !long {
			outputLine(line, names, values)
		}
		line, long = line[:0], false
	}
}

// outputLine reads one complete line into values when it is an output line of one of names.
func outputLine(line []byte, names []string, values map[string]string) {
	rest, ok := bytes.CutPrefix(line, []byte(ci.OutputPrefix))
	if !ok {
		return
	}
	name, value, ok := strings.Cut(strings.TrimRight(string(rest), "\r\n"), " ")
	if !ok || !slices.Contains(names, name) {
		return
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" {
		return
	}
	values[name] = string(data)
}
