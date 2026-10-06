package errorlog

import (
	"bytes"
	"io"
	"strings"
)

// LineWriter wraps the writer of the standard library logger: every line that reads like an error is counted, every line is passed on.
// The agent logs with the standard logger.
type LineWriter struct {
	agg  *Aggregator
	next io.Writer
}

// NewLineWriter counts the error-looking lines written to it into agg and writes all of them to next.
func NewLineWriter(agg *Aggregator, next io.Writer) *LineWriter {
	return &LineWriter{agg: agg, next: next}
}

var errorWords = []string{"error", "failed", "panic", "fatal", "cannot ", "unable to", "refused", "denied"}

func looksLikeError(line string) bool {
	l := strings.ToLower(line)
	for _, w := range errorWords {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

func (w *LineWriter) Write(p []byte) (int, error) {
	for _, line := range bytes.Split(p, []byte("\n")) {
		if s := strings.TrimSpace(string(line)); s != "" && looksLikeError(s) {
			w.agg.Record("log", s)
		}
	}
	return w.next.Write(p)
}
