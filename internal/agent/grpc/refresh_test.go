package grpc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	labv1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	labfake "github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	typed "github.com/cybericebox/laboratory/clientset/client/versioned/typed/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

type refreshVersioned struct {
	versioned.Interface
	api typed.LaboratoryV1alpha1Interface
}

func (c refreshVersioned) LaboratoryV1alpha1() typed.LaboratoryV1alpha1Interface { return c.api }

type refreshAPI struct {
	typed.LaboratoryV1alpha1Interface
	tenants typed.TenantInterface
}

func (c refreshAPI) Tenants() typed.TenantInterface { return c.tenants }

type refreshTenants struct {
	typed.TenantInterface
	before func(context.Context) error
}

func (c refreshTenants) Get(ctx context.Context, name string, o metav1.GetOptions) (*labv1.Tenant, error) {
	if err := c.before(ctx); err != nil {
		return nil, err
	}
	return c.TenantInterface.Get(ctx, name, o)
}
func refreshHandler(before func(context.Context) error) *Handler {
	cs := labfake.NewSimpleClientset(&labv1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: names.DefaultTenant}})
	api := refreshAPI{LaboratoryV1alpha1Interface: cs.LaboratoryV1alpha1(), tenants: refreshTenants{TenantInterface: cs.LaboratoryV1alpha1().Tenants(), before: before}}
	return NewHandler(refreshVersioned{Interface: cs, api: api}, k8sfake.NewSimpleClientset(), nil)
}
func refreshCall(h *Handler, features bool, ctx context.Context) error {
	if features {
		_, err := h.tenantFeatures(ctx)
		return err
	}
	_, err := h.tenantCapacity(ctx)
	return err
}

func TestConcurrentTenantRefreshCoalesces(t *testing.T) {
	for _, features := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity", true: "features"}[features], func(t *testing.T) {
			gate := make(chan struct{})
			first := make(chan struct{})
			var calls atomic.Int64
			h := refreshHandler(func(ctx context.Context) error {
				if calls.Add(1) == 1 {
					close(first)
				}
				select {
				case <-gate:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			var wg sync.WaitGroup
			errs := make(chan error, 21)
			wg.Add(1)
			go func() { defer wg.Done(); errs <- refreshCall(h, features, context.Background()) }()
			<-first
			ready := make(chan struct{}, 20)
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); ready <- struct{}{}; errs <- refreshCall(h, features, context.Background()) }()
			}
			for i := 0; i < 20; i++ {
				<-ready
			}
			time.Sleep(30 * time.Millisecond)
			close(gate)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("simultaneous tenant refresh made %d reads, want1", got)
			}
		})
	}
}

func TestCanceledTenantRefreshWaiterDoesNotRead(t *testing.T) {
	for _, features := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity", true: "features"}[features], func(t *testing.T) {
			gate := make(chan struct{})
			first := make(chan struct{})
			var calls atomic.Int64
			h := refreshHandler(func(ctx context.Context) error {
				if calls.Add(1) == 1 {
					close(first)
				}
				select {
				case <-gate:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			leader := make(chan error, 1)
			go func() { leader <- refreshCall(h, features, context.Background()) }()
			<-first
			ctx, cancel := context.WithCancel(context.Background())
			waiter := make(chan error, 1)
			go func() { waiter <- refreshCall(h, features, ctx) }()
			time.Sleep(20 * time.Millisecond)
			cancel()
			select {
			case err := <-waiter:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("waiter ignored cancellation")
			}
			close(gate)
			if err := <-leader; err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("canceled waiter entered API refresh")
			}
		})
	}
}

func TestCanceledTenantRefreshLeaderDoesNotCancelWaiter(t *testing.T) {
	for _, features := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity", true: "features"}[features], func(t *testing.T) {
			first := make(chan struct{})
			second := make(chan struct{})
			var calls atomic.Int64
			h := refreshHandler(func(ctx context.Context) error {
				if calls.Add(1) == 1 {
					close(first)
					<-ctx.Done()
					return ctx.Err()
				}
				close(second)
				return nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			leader := make(chan error, 1)
			go func() { leader <- refreshCall(h, features, ctx) }()
			<-first
			waiter := make(chan error, 1)
			go func() { waiter <- refreshCall(h, features, context.Background()) }()
			cancel()
			if err := <-leader; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			select {
			case <-second:
			case <-time.After(time.Second):
				t.Fatal("live waiter did not retry")
			}
			if err := <-waiter; err != nil {
				t.Fatalf("live waiter inherited leader failure: %v", err)
			}
			if calls.Load() != 2 {
				t.Fatal("failed refresh was cached")
			}
		})
	}
}

func TestTenantRefreshDoesNotBlockOtherTenants(t *testing.T) {
	for _, features := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity", true: "features"}[features], func(t *testing.T) {
			started := make(chan string, 2)
			release := make(chan struct{})
			h := refreshHandler(func(ctx context.Context) error { started <- tenantOf(ctx); <-release; return nil })
			done := make(chan error, 2)
			go func() { done <- refreshCall(h, features, asClient("a")) }()
			<-started
			go func() { done <- refreshCall(h, features, asClient("b")) }()
			select {
			case got := <-started:
				if got != "b" {
					t.Error("wrong tenant", got)
				}
			case <-time.After(time.Second):
				t.Error("other tenant serialized behind active refresh")
			}
			close(release)
			for i := 0; i < 2; i++ {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTenantRefreshStillExpires(t *testing.T) {
	for _, features := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity", true: "features"}[features], func(t *testing.T) {
			var calls atomic.Int64
			h := refreshHandler(func(context.Context) error { calls.Add(1); return nil })
			if err := refreshCall(h, features, context.Background()); err != nil {
				t.Fatal(err)
			}
			if features {
				h.featCache.mu.Lock()
				e := h.featCache.m[names.DefaultTenant]
				e.at = time.Now().Add(-capacityTTL)
				h.featCache.m[names.DefaultTenant] = e
				h.featCache.mu.Unlock()
			} else {
				h.capCache.mu.Lock()
				e := h.capCache.m[names.DefaultTenant]
				e.at = time.Now().Add(-capacityTTL)
				h.capCache.m[names.DefaultTenant] = e
				h.capCache.mu.Unlock()
			}
			if err := refreshCall(h, features, context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatal("expired tenant view was reused")
			}
		})
	}
}
