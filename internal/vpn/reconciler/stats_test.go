package reconciler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

type fakeDevice struct {
	mu   sync.Mutex
	peer wgtypes.Peer
}

func (d *fakeDevice) Device() (*wgtypes.Device, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return &wgtypes.Device{Peers: []wgtypes.Peer{d.peer}}, nil
}

func (d *fakeDevice) handshake(at time.Time, rx int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.peer.LastHandshakeTime, d.peer.ReceiveBytes = at, rx
}

func statsFixture(t *testing.T) (client.Client, *fakeDevice, *atomic.Int32) {
	t.Helper()
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	if err := lab.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	lgc := &lab.LabGroupClient{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "g"},
		Spec:       lab.LabGroupClientSpec{PublicKey: key.PublicKey().String()},
	}
	var patches atomic.Int32
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lgc).WithStatusSubresource(lgc).
		WithInterceptorFuncs(interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, o ...client.SubResourcePatchOption) error {
			patches.Add(1)
			return c.SubResource(sub).Patch(ctx, obj, p, o...)
		}}).Build()
	return c, &fakeDevice{peer: wgtypes.Peer{PublicKey: key.PublicKey()}}, &patches
}

func lastHandshake(t *testing.T, c client.Client) time.Time {
	t.Helper()
	var got lab.LabGroupClient
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "g", Name: "alice"}, &got); err != nil {
		t.Fatal(err)
	}
	return got.Status.Statistics.LastHandshake.Time
}

// A client that connects between two statistics ticks shows its handshake within the short handshake interval, not at the next tick.
func TestRunStatsPublishesANewHandshakeBeforeTheStatsTick(t *testing.T) {
	c, dev, _ := statsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunStats(ctx, c, dev, "g", time.Hour, 20*time.Millisecond)

	at := time.Now().Truncate(time.Second)
	dev.handshake(at, 100)
	deadline := time.Now().Add(2 * time.Second)
	for lastHandshake(t, c).IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("a new handshake was not published before the statistics tick")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := lastHandshake(t, c); !got.Equal(at) {
		t.Fatalf("lastHandshake %v, want %v", got, at)
	}
}

// The short interval writes a client only when its handshake moved: traffic alone waits for the statistics tick.
func TestRunStatsHandshakeCheckWritesOnlyOnChange(t *testing.T) {
	c, dev, patches := statsFixture(t)
	dev.handshake(time.Now().Truncate(time.Second), 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunStats(ctx, c, dev, "g", time.Hour, 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for lastHandshake(t, c).IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("the handshake was not published")
		}
		time.Sleep(10 * time.Millisecond)
	}
	dev.handshake(lastHandshake(t, c), 5000) // more traffic, the same handshake
	time.Sleep(200 * time.Millisecond)
	if n := patches.Load(); n != 1 {
		t.Fatalf("%d status writes, want 1 (only the new handshake)", n)
	}
}
