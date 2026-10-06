package l7

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestLogReportFailureKeepsRefusalsOutOfTheErrorLog(t *testing.T) {
	var errs int
	sink := funcr.New(func(prefix, args string) {}, funcr.Options{Verbosity: 1})
	counting := logr.New(&countErrors{LogSink: sink.GetSink(), n: &errs})
	gr := schema.GroupResource{Group: "laboratory.cybericebox.com", Resource: "labtrafficreports"}
	fn := LogReportFailure(counting)

	fn(fmt.Errorf("get traffic report: %w", apierrors.NewForbidden(gr, "proxy-x", errors.New("no binding yet"))))
	fn(fmt.Errorf("get traffic report: %w", apierrors.NewNotFound(gr, "proxy-x")))
	if errs != 0 {
		t.Fatalf("forbidden and not found are not errors, got %d", errs)
	}
	fn(errors.New("boom"))
	if errs != 1 {
		t.Fatalf("other failures stay errors, got %d", errs)
	}
}

type countErrors struct {
	logr.LogSink
	n *int
}

func (c *countErrors) Error(err error, msg string, kv ...any) { *c.n++ }
