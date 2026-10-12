package devicestate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/go-logr/logr/funcr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cybericebox/laboratory/internal/errorlog"
)

type exitTestSubscription struct {
	events  chan *events.Envelope
	errors  chan error
	filters []string
}

type exitTestService struct {
	subscriptions chan exitTestSubscription
}

func (s *exitTestService) Publish(context.Context, string, events.Event) error {
	return errors.New("test service only supports subscriptions")
}

func (s *exitTestService) Forward(context.Context, *events.Envelope) error {
	return errors.New("test service only supports subscriptions")
}

func (s *exitTestService) Subscribe(ctx context.Context, filters ...string) (<-chan *events.Envelope, <-chan error) {
	sub := exitTestSubscription{
		events: make(chan *events.Envelope, 128),
		errors: make(chan error, 1), filters: append([]string(nil), filters...),
	}
	select {
	case s.subscriptions <- sub:
	case <-ctx.Done():
		close(sub.errors)
	}
	return sub.events, sub.errors
}

type exitTestFixture struct {
	service *exitTestService
	journal *errorlog.Aggregator
	logs    chan string
	out     <-chan string
	cancel  context.CancelFunc
}

func newExitTestFixture(t *testing.T) *exitTestFixture {
	t.Helper()
	f := &exitTestFixture{
		service: &exitTestService{subscriptions: make(chan exitTestSubscription, 2)},
		journal: errorlog.New("node-agent", "test-node"),
		logs:    make(chan string, 8),
	}
	client, err := containerd.New("", containerd.WithServices(containerd.WithEventService(f.service)))
	if err != nil {
		t.Fatal(err)
	}
	base := funcr.New(func(prefix, args string) { f.logs <- prefix + args }, funcr.Options{})
	runtime := &ContainerdRuntime{client: client, namespace: "k8s.io", log: errorlog.WrapLogger(base, f.journal)}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.out, err = runtime.Exits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		waitExitTestClosed(t, f.out)
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func nextExitTestSubscription(t *testing.T, f *exitTestFixture) exitTestSubscription {
	t.Helper()
	select {
	case sub := <-f.service.subscriptions:
		if len(sub.filters) != 1 || sub.filters[0] != "topic==\"/tasks/exit\",namespace==\"k8s.io\"" {
			t.Fatalf("init exits must stay scoped to the CRI namespace: %v", sub.filters)
		}
		return sub
	case <-time.After(6 * time.Second):
		t.Fatal("exit subscription was not established")
		return exitTestSubscription{}
	}
}

func nextExitTestLog(t *testing.T, f *exitTestFixture) string {
	t.Helper()
	select {
	case line := <-f.logs:
		return line
	case <-time.After(time.Second):
		t.Fatal("stream termination lost its diagnostic log")
		return ""
	}
}

func exitTestEvent(t *testing.T, containerID, processID string) *events.Envelope {
	t.Helper()
	at := time.Unix(1700000000, 0)
	event, err := typeurl.MarshalAny(&eventtypes.TaskExit{
		ContainerID: containerID, ID: processID, Pid: 42, ExitStatus: 0, ExitedAt: timestamppb.New(at),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &events.Envelope{Timestamp: at, Namespace: "k8s.io", Topic: "/tasks/exit", Event: event}
}

func nextExitTestID(t *testing.T, out <-chan string, want string) {
	t.Helper()
	select {
	case got, ok := <-out:
		if !ok || got != want {
			t.Fatalf("exit ID = %q, open = %v, want %q", got, ok, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("init exit %q was not delivered", want)
	}
}

func waitExitTestClosed(t *testing.T, out <-chan string) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-out:
			if !ok {
				return
			}
		case <-deadline.C:
			t.Error("canceling the exit subscription did not close its output")
			return
		}
	}
}

func TestContainerdExitStreamErrorClassification(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		critical bool
	}{
		{"EOF", io.EOF, false},
		{"wrapped EOF", fmt.Errorf("receive: %w", io.EOF), false},
		{"gRPC unavailable", status.Error(codes.Unavailable, "transport closed"), false},
		{"wrapped gRPC unavailable", fmt.Errorf("receive: %w", status.Error(codes.Unavailable, "transport closed")), false},
		{"native unavailable", errdefs.ErrUnavailable, false},
		{"gRPC canceled", status.Error(codes.Canceled, "stream stopped"), false},
		{"context canceled", context.Canceled, false},
		{"permission denied", status.Error(codes.PermissionDenied, "subscription denied"), true},
		{"native permission denied", errdefs.ErrPermissionDenied, true},
		{"unauthenticated", status.Error(codes.Unauthenticated, "credentials rejected"), true},
		{"invalid request", status.Error(codes.InvalidArgument, "invalid event filter"), true},
		{"data loss", status.Error(codes.DataLoss, "malformed message"), true},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"unknown", errors.New("malformed event transport"), true},
		{"EOF text in an unknown error", errors.New("decode failed: EOF in header"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExitTestFixture(t)
			sub := nextExitTestSubscription(t, f)
			sub.errors <- tc.err
			line := nextExitTestLog(t, f)
			if !strings.Contains(line, tc.err.Error()) {
				t.Fatalf("stream error diagnostics were lost: %s", line)
			}
			groups := f.journal.Snapshot()
			if tc.critical {
				if len(groups) != 1 || groups[0].Total != 1 {
					t.Fatalf("permanent failure must reach the error journal once: %+v", groups)
				}
			} else if len(groups) != 0 {
				t.Fatalf("recoverable disconnect must not reach the critical error journal: %+v", groups)
			}
		})
	}
}

func TestContainerdExitStreamReconnectDeliversInitExits(t *testing.T) {
	f := newExitTestFixture(t)
	first := nextExitTestSubscription(t, f)
	first.events <- exitTestEvent(t, "first-container", "exec-process")
	first.events <- exitTestEvent(t, "first-container", "first-container")
	nextExitTestID(t, f.out, "first-container")
	disconnected := time.Now()
	first.errors <- io.EOF
	nextExitTestLog(t, f)
	second := nextExitTestSubscription(t, f)
	if time.Since(disconnected) < 2*time.Second {
		t.Fatal("a disconnect must keep the reconnect backoff instead of hot-looping")
	}
	second.events <- exitTestEvent(t, "second-container", "second-container")
	nextExitTestID(t, f.out, "second-container")
	if groups := f.journal.Snapshot(); len(groups) != 0 {
		t.Fatalf("successful reconnect left a false critical error: %+v", groups)
	}
}

func TestContainerdExitStreamCancellationClosesOutput(t *testing.T) {
	for _, phase := range []string{"receiving", "backoff", "blocked delivery"} {
		t.Run(phase, func(t *testing.T) {
			f := newExitTestFixture(t)
			sub := nextExitTestSubscription(t, f)
			switch phase {
			case "backoff":
				sub.errors <- status.Error(codes.Unavailable, "transport closed")
				nextExitTestLog(t, f)
			case "blocked delivery":
				for i := 0; i < cap(f.out)+1; i++ {
					sub.events <- exitTestEvent(t, "container", "container")
				}
				deadline := time.Now().Add(time.Second)
				for len(f.out) < cap(f.out) && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if len(f.out) != cap(f.out) {
					t.Fatal("fixture did not block exit delivery")
				}
			}
			f.cancel()
			waitExitTestClosed(t, f.out)
			if groups := f.journal.Snapshot(); len(groups) != 0 {
				t.Fatalf("normal shutdown must not create critical errors: %+v", groups)
			}
		})
	}
}
