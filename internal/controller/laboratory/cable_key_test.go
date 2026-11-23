package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The operator makes the cable key once: a missing Secret gets a 64-character key, an existing one is never changed.
func TestCableKeyEnsurerMakesTheKeyOnce(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	e := &CableKeyEnsurer{Client: c, Namespace: "laboratory-device-access"}
	ctx := context.Background()
	if err := e.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "laboratory-device-access", Name: CableKeySecret}, &s); err != nil || len(s.Data["key"]) != 64 {
		t.Fatalf("%v %q", err, s.Data["key"])
	}
	first := string(s.Data["key"])
	if err := e.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "laboratory-device-access", Name: CableKeySecret}, &s)
	if string(s.Data["key"]) != first {
		t.Fatal("an existing key was changed")
	}
	// a rotated Secret (key and previous set by hand) is left alone too
	s.Data = map[string][]byte{"key": []byte("new"), "previous": []byte(first)}
	_ = c.Update(ctx, &s)
	_ = e.Ensure(ctx)
	_ = c.Get(ctx, types.NamespacedName{Namespace: "laboratory-device-access", Name: CableKeySecret}, &s)
	if string(s.Data["key"]) != "new" || string(s.Data["previous"]) != first {
		t.Fatal("a rotated key was changed")
	}
}
