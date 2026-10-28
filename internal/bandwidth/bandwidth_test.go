package bandwidth

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/config"

	"context"
)

func TestComputeMatchesTheDesignTable(t *testing.T) {
	c := DefaultConfig()
	// xlarge: 1562 - 100 system - 100 platform = 1362
	n := c.Compute(1562, []int64{50, 20, 30})
	if n.UKbps != 1362000 || n.EntryKbps != 681000 || n.InternetKbps != 340500 {
		t.Fatalf("classes: %+v", n)
	}
	if want := int64(340500 * 0.85); n.GeneveKbps != want {
		t.Fatalf("geneve class %d, want %d (overhead factor applied)", n.GeneveKbps, want)
	}
	// no label: the default 1000 Mbit/s
	if n := c.Compute(0, nil); n.UKbps != 900000 {
		t.Fatalf("default node: %+v", n)
	}
	// reserves larger than the node leave nothing, never a negative rate
	if n := c.Compute(1000, []int64{900, 900}); n.UKbps != 0 || n.EntryKbps != 0 || n.GeneveKbps != 0 {
		t.Fatalf("over-reserved node: %+v", n)
	}
	// a negative reserve is ignored, not added
	if n := c.Compute(1000, []int64{-50}); n.UKbps != 900000 {
		t.Fatalf("negative reserve: %+v", n)
	}
}

func TestFairShare(t *testing.T) {
	cases := []struct {
		class, cap int64
		active     int
		want       int64
	}{
		{681000, 50000, 16, 42562}, // the 16 participants of an xlarge: an equal part
		{681000, 50000, 2, 50000},  // few active: the cap
		{681000, 50000, 0, 50000},  // nobody counted is treated as one
		{681000, 0, 4, 170250},     // no cap
		{0, 50000, 100, 50000},     // no class: only the cap
		{0, 0, 3, 0},               // nothing set: unlimited
		{10, 0, 1000, 1},           // never rounds down to unlimited
	}
	for _, tc := range cases {
		if got := FairShare(tc.class, tc.cap, tc.active); got != tc.want {
			t.Errorf("FairShare(%d,%d,%d) = %d, want %d", tc.class, tc.cap, tc.active, got, tc.want)
		}
	}
}

func TestLabAndNodeGeneveRates(t *testing.T) {
	c := DefaultConfig()
	n := c.Compute(1562, []int64{100}) // U 1362, geneve class 1362*25/100*0.85 = 289425
	if got := c.LabGeneveKbps(n, 12, 1); got != 24118 {
		t.Fatalf("12 split labs, one remote node: %d", got)
	}
	if got := c.LabGeneveKbps(n, 12, 2); got != 12059 {
		t.Fatalf("a frame flooded to two nodes counts twice, so the rate halves: %d", got)
	}
	if got := c.LabGeneveKbps(n, 1, 1); got != 100000 {
		t.Fatalf("one split lab is held to the lab cap: %d", got)
	}
	if got := NodeGeneveKbps(n, 3); got != 96475 {
		t.Fatalf("node meter: %d", got)
	}
	if got := c.LabInternetKbps(n, 100); got != 3405 {
		t.Fatalf("100 labs share the internet class: %d", got)
	}
}

func TestInvariantOnEveryNode(t *testing.T) {
	c := DefaultConfig()
	for _, bw := range []int64{1000, 1562, 3125, 6250} {
		for _, labs := range []int{1, 5, 40, 230} {
			n := c.Compute(bw, []int64{100})
			sum := n.EntryKbps + n.InternetKbps + n.GeneveKbps
			if sum > n.UKbps {
				t.Fatalf("bw %d: classes %d exceed U %d", bw, sum, n.UKbps)
			}
			if got := c.LabInternetKbps(n, labs) * int64(labs); got > n.InternetKbps && labs > 0 && n.InternetKbps/int64(labs) >= 1 {
				t.Fatalf("bw %d labs %d: lab ceilings %d exceed the internet class %d", bw, labs, got, n.InternetKbps)
			}
		}
	}
}

func TestParseMbit(t *testing.T) {
	for in, want := range map[string]int64{"1562": 1562, "1562M": 1562, " 10G ": 10000, "1g": 1000} {
		if got, ok := ParseMbit(in); !ok || got != want {
			t.Errorf("ParseMbit(%q) = %d,%v", in, got, ok)
		}
	}
	for _, in := range []string{"", "x", "0", "-5", "1.5G", "M"} {
		if _, ok := ParseMbit(in); ok {
			t.Errorf("ParseMbit(%q) should fail", in)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := DefaultConfig()
	bad.GeneveShare = 30
	if bad.Validate() == nil {
		t.Fatal("shares over 100 must be refused")
	}
	bad = DefaultConfig()
	bad.GeneveOverhead = 0
	if bad.Validate() == nil {
		t.Fatal("a zero overhead factor must be refused")
	}
}

func TestLimitsRoundTrip(t *testing.T) {
	in := Limits{ClassKbps: 340000, CapKbps: 50000}
	out, err := Decode(in.Encode())
	if err != nil || out.ClassKbps != 340000 || out.CapKbps != 50000 || out.V != Version {
		t.Fatalf("%+v %v", out, err)
	}
	if l, err := Decode(""); err != nil || !l.Zero() {
		t.Fatalf("empty: %+v %v", l, err)
	}
	for _, bad := range []string{`{"v":2}`, `{"v":1,"capKbps":-1}`, `not json`} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("Decode(%q) should fail", bad)
		}
	}
}

func TestParseAnnotationsAndWatch(t *testing.T) {
	data := "a=\"b\"\ncybericebox.com/bandwidth-limits=\"{\\\"v\\\":1,\\\"capKbps\\\":50000}\"\nbroken\n"
	if got := ParseAnnotations(data)[annotationKey]; got != `{"v":1,"capKbps":50000}` {
		t.Fatalf("annotation: %q", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "annotations")
	if l, err := ReadLimits(path); err != nil || !l.Zero() {
		t.Fatalf("missing file: %+v %v", l, err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Limits, 4)
	go func() { _ = Watch(ctx, path, func(l Limits) { got <- l }, nil) }()
	if l := recv(t, got); l.CapKbps != 50000 {
		t.Fatalf("first apply: %+v", l)
	}
	next := "cybericebox.com/bandwidth-limits=\"{\\\"v\\\":1,\\\"capKbps\\\":20000}\"\n"
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}
	if l := recv(t, got); l.CapKbps != 20000 {
		t.Fatalf("after the change: %+v", l)
	}
}

func recv(t *testing.T, ch <-chan Limits) Limits {
	t.Helper()
	select {
	case l := <-ch:
		return l
	case <-time.After(5 * time.Second):
		t.Fatal("no limits applied in time")
		return Limits{}
	}
}

func TestForNodeCountsOnlyThePodsOfThatNode(t *testing.T) {
	c := DefaultConfig()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{names.LabelNodeBandwidth: "1562"}}}
	pod := func(node, reserve string, phase corev1.PodPhase) corev1.Pod {
		p := corev1.Pod{Spec: corev1.PodSpec{NodeName: node}, Status: corev1.PodStatus{Phase: phase}}
		if reserve != "" {
			p.Annotations = map[string]string{names.AnnotationBandwidthReserve: reserve}
		}
		return p
	}
	pods := []corev1.Pod{
		pod("n1", "50", corev1.PodRunning),
		pod("n1", "20", corev1.PodPending),
		pod("n2", "500", corev1.PodRunning),   // another node: not subtracted here
		pod("n1", "300", corev1.PodSucceeded), // finished
		pod("n1", "", corev1.PodRunning),      // declares nothing
		pod("n1", "junk", corev1.PodRunning),
	}
	if got := c.ForNode(node, pods).UKbps; got != (1562-100-50-20)*1000 {
		t.Fatalf("U = %d", got)
	}
	bare := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n3"}}
	if got := c.ForNode(bare, nil).UKbps; got != 900_000 {
		t.Fatalf("a node without the label is 1 Gbit/s: %d", got)
	}
}

func TestNodeGeneveLimits(t *testing.T) {
	c := DefaultConfig()
	l := c.NodeGeneveLimits(c.Compute(1562, nil))
	if l.GeneveKbps != 310_675 || l.LabCapKbps != 100_000 || l.V != Version {
		t.Fatalf("%+v", l)
	}
}

func TestSettingsReadFromTheEnvironment(t *testing.T) {
	var cfg struct {
		Bandwidth Settings
	}
	if err := config.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Bandwidth.Enabled || cfg.Bandwidth.DefaultMbit != 1000 || cfg.Bandwidth.GeneveOverhead != 0.85 {
		t.Fatalf("defaults: %+v", cfg.Bandwidth)
	}
	if cfg.Bandwidth.Config().Validate() != nil || cfg.Bandwidth.Config() != DefaultConfig() {
		t.Fatalf("the defaults are the default config: %+v", cfg.Bandwidth.Config())
	}
	t.Setenv("BANDWIDTH_ENABLED", "true")
	t.Setenv("BANDWIDTH_GENEVE_SHARE", "10")
	t.Setenv("BANDWIDTH_PARTICIPANT_CAP_MBIT", "20")
	if err := config.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Bandwidth.Enabled || cfg.Bandwidth.GeneveShare != 10 || cfg.Bandwidth.ParticipantCapMbit != 20 {
		t.Fatalf("from the environment: %+v", cfg.Bandwidth)
	}
}

func TestWatchNodeReadsTheKeyOfItsNode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n1"), []byte(`{"v":1,"classKbps":300000,"capKbps":50000}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "n2"), []byte(`{"v":1,"classKbps":1,"capKbps":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Limits, 4)
	go func() { _ = WatchNode(ctx, dir, "n1", func(l Limits) { got <- l }, nil) }()
	if l := recv(t, got); l.ClassKbps != 300000 || l.CapKbps != 50000 {
		t.Fatalf("the key of n1: %+v", l)
	}
	if err := os.WriteFile(filepath.Join(dir, "n1"), []byte(`{"v":1,"classKbps":100000,"capKbps":20000}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if l := recv(t, got); l.ClassKbps != 100000 {
		t.Fatalf("after the change: %+v", l)
	}
}

func TestWatchWithoutTheDirectoryAppliesNothingOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan Limits, 2)
	go func() {
		_ = WatchNode(ctx, filepath.Join(t.TempDir(), "absent"), "n1", func(l Limits) { got <- l }, nil)
	}()
	if l := recv(t, got); !l.Zero() {
		t.Fatalf("%+v", l)
	}
	cancel()
}
