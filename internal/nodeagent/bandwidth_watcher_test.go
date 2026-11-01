package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/bandwidth"
)

type limiterSpy struct {
	mu  sync.Mutex
	got []bandwidth.Limits
}

func (l *limiterSpy) SetGeneveLimits(x bandwidth.Limits) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, x)
	return nil
}

func (l *limiterSpy) last(t *testing.T, n int) bandwidth.Limits {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		if len(l.got) >= n {
			x := l.got[len(l.got)-1]
			l.mu.Unlock()
			return x
		}
		l.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d applications", n)
	return bandwidth.Limits{}
}

func TestWatcherAppliesTheKeyOfItsOwnNode(t *testing.T) {
	dir := t.TempDir()
	write := func(node, v string) {
		if err := os.WriteFile(filepath.Join(dir, node), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("n1", `{"v":1,"classKbps":300000,"capKbps":50000,"geneveKbps":289425,"labCapKbps":100000}`)
	write("n2", `{"v":1,"geneveKbps":1,"labCapKbps":1}`)
	spy := &limiterSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &BandwidthWatcher{NodeName: "n1", Dir: dir, Flows: spy}
	go func() { _ = w.Start(ctx) }()
	if l := spy.last(t, 1); l.GeneveKbps != 289425 || l.LabCapKbps != 100000 {
		t.Fatalf("the Geneve numbers of n1: %+v", l)
	}
	write("n1", `{"v":1,"geneveKbps":100000,"labCapKbps":50000}`)
	if l := spy.last(t, 2); l.GeneveKbps != 100000 || l.LabCapKbps != 50000 {
		t.Fatalf("after the operator changed them: %+v", l)
	}
}

func TestWatcherWithoutTheVolumeAppliesNoLimits(t *testing.T) {
	spy := &limiterSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = (&BandwidthWatcher{NodeName: "n1", Dir: filepath.Join(t.TempDir(), "absent"), Flows: spy}).Start(ctx)
	}()
	if l := spy.last(t, 1); !l.Zero() {
		t.Fatalf("%+v", l)
	}
}
