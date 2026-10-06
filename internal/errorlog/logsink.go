package errorlog

import (
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Sink is a logr sink that counts every error-level message into an Aggregator and passes everything on to the real sink. The
// error path of any controller-runtime based component is then in the journal without touching its code.
type Sink struct {
	inner logr.LogSink
	agg   *Aggregator
	names []string
}

// WrapLogger returns a logger that records its errors in agg and logs as l does.
func WrapLogger(l logr.Logger, agg *Aggregator) logr.Logger {
	return logr.New(&Sink{inner: l.GetSink(), agg: agg})
}

func (s *Sink) Init(info logr.RuntimeInfo) { s.inner.Init(info) }

func (s *Sink) Enabled(level int) bool { return s.inner.Enabled(level) }

func (s *Sink) Info(level int, msg string, kv ...any) { s.inner.Info(level, msg, kv...) }

func (s *Sink) Error(err error, msg string, kv ...any) {
	// An optimistic-concurrency conflict is not a fault: the caller read a stale copy and the work is retried with a fresh
	// one. It is logged for the trace and kept out of the error journal.
	if err != nil && apierrors.IsConflict(err) {
		s.inner.Info(0, msg+" (conflict, will retry)", append([]any{"error", err.Error()}, kv...)...)
		return
	}
	text := msg
	if err != nil {
		text = fmt.Sprintf("%s: %v", msg, err)
	}
	s.agg.Record(s.kind(), text)
	s.inner.Error(err, msg, kv...)
}

func (s *Sink) WithValues(kv ...any) logr.LogSink {
	return &Sink{inner: s.inner.WithValues(kv...), agg: s.agg, names: s.names}
}

func (s *Sink) WithName(name string) logr.LogSink {
	return &Sink{inner: s.inner.WithName(name), agg: s.agg, names: append(append([]string{}, s.names...), name)}
}

// kind is the first name of the logger ("labgroup", "scheduler", "setup-networks", ...): the path the error came from.
func (s *Sink) kind() string {
	if len(s.names) == 0 {
		return "log"
	}
	return strings.ToLower(s.names[0])
}
