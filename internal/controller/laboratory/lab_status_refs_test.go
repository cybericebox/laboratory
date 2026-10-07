package laboratory

import (
	"context"
	"errors"
	"testing"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type refsOrderClient struct {
	client.Client
	updates    int
	reverse    bool
	failUpdate bool
}

func (c *refsOrderClient) List(ctx context.Context, out client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, out, opts...); err != nil {
		return err
	}
	if c.reverse {
		switch list := out.(type) {
		case *api.DeviceList:
			for i, j := 0, len(list.Items)-1; i < j; i, j = i+1, j-1 {
				list.Items[i], list.Items[j] = list.Items[j], list.Items[i]
			}
		case *api.ConnectionList:
			for i, j := 0, len(list.Items)-1; i < j; i, j = i+1, j-1 {
				list.Items[i], list.Items[j] = list.Items[j], list.Items[i]
			}
		}
	}
	return nil
}
func (c *refsOrderClient) Status() client.SubResourceWriter {
	return &refsStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

type refsStatusWriter struct {
	client.SubResourceWriter
	owner *refsOrderClient
}

func (w *refsStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*api.Lab); ok {
		w.owner.updates++
		if w.owner.failUpdate {
			w.owner.failUpdate = false
			return errors.New("status unavailable")
		}
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}
func TestLabStatusListOrderAloneDoesNotWrite(t *testing.T) {
	r, lab, base := snapshotReconcileFixture(t, 4)
	if err := snapshotReconcile(t, r, lab); err != nil {
		t.Fatal(err)
	}
	c := &refsOrderClient{Client: base, reverse: true}
	r.Client = c
	var current api.Lab
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(lab), &current); err != nil {
		t.Fatal(err)
	}
	previous := current.Status.Devices
	if _, err := r.updateStatus(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if c.updates != 0 {
		t.Fatalf("list order alone caused%d Lab status writes", c.updates)
	}
	for i := range previous {
		if current.Status.Devices[i].Name != previous[i].Name {
			t.Fatal("published order changed")
		}
	}
}

func TestLabStatusActualRefChangeWritesAndRetries(t *testing.T) {
	r, lab, base := snapshotReconcileFixture(t, 2)
	if err := snapshotReconcile(t, r, lab); err != nil {
		t.Fatal(err)
	}
	var list api.DeviceList
	_ = base.List(context.Background(), &list)
	list.Items[0].Status.Ready = true
	if err := base.Client.Status().Update(context.Background(), &list.Items[0]); err != nil {
		t.Fatal(err)
	}
	c := &refsOrderClient{Client: base, reverse: true, failUpdate: true}
	r.Client = c
	var current api.Lab
	_ = base.Get(context.Background(), client.ObjectKeyFromObject(lab), &current)
	if _, err := r.updateStatus(context.Background(), &current); err == nil {
		t.Fatal("real status update error hidden")
	}
	_ = base.Get(context.Background(), client.ObjectKeyFromObject(lab), &current)
	if _, err := r.updateStatus(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if c.updates != 2 {
		t.Fatalf("update attempts=%d", c.updates)
	}
	foundReady := false
	for _, ref := range current.Status.Devices {
		foundReady = foundReady || ref.Ready
	}
	if !foundReady {
		t.Fatal("changed readiness lost")
	}
}

func TestLabRefEqualityPreservesMultiplicityAndCompleteValues(t *testing.T) {
	a := api.DeviceRef{Name: "a"}
	b := api.DeviceRef{Name: "b", Ready: true}
	changed := api.DeviceRef{Name: "a", Failure: &api.PodFailure{Reason: "failed", Message: "new detail"}}
	for _, tt := range []struct {
		name        string
		left, right []api.DeviceRef
		want        bool
	}{
		{"reverse", []api.DeviceRef{a, b}, []api.DeviceRef{b, a}, true},
		{"rotate", []api.DeviceRef{a, b, a}, []api.DeviceRef{a, a, b}, true},
		{"different duplicate", []api.DeviceRef{a, a}, []api.DeviceRef{a, b}, false},
		{"extra duplicate", []api.DeviceRef{a, a, b}, []api.DeviceRef{a, b}, false},
		{"full value", []api.DeviceRef{a, b}, []api.DeviceRef{b, changed}, false},
		{"nil distinct", nil, []api.DeviceRef{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameRefsByName(tt.left, tt.right, func(ref api.DeviceRef) string { return ref.Name }); got != tt.want {
				t.Fatalf("got%t want%t", got, tt.want)
			}
		})
	}
	left := []api.ConnectionRef{{Name: "a", Ready: true}, {Name: "b"}, {Name: "a", Ready: false}}
	right := []api.ConnectionRef{{Name: "a", Ready: false}, {Name: "a", Ready: true}, {Name: "b"}}
	if !sameRefsByName(left, right, func(ref api.ConnectionRef) string { return ref.Name }) {
		t.Fatal("connection duplicate values lost")
	}
	right[1].Ready = false
	if sameRefsByName(left, right, func(ref api.ConnectionRef) string { return ref.Name }) {
		t.Fatal("connection readiness multiplicity collapsed")
	}
}
