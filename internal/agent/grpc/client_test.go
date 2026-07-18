package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// CreateLabGroupClient generates the keypair, registers the public key, waits
// for the reconciler to assemble the config, and substitutes the real private
// key for the placeholder. envtest has no reconciler, so a goroutine simulates
// it (assigns an IP + writes a placeholder config into status).
func TestCreateLabGroupClient(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	mustNamespace(t, k8s, "team-cli")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-cli").Get(ctx, "alice", metav1.GetOptions{})
			if err == nil {
				c.Status.AssignedIP = "10.8.0.5/32"
				c.Status.Config = "[Interface]\nPrivateKey = " + names.WGPrivateKeyPlaceholder + "\nAddress = 10.8.0.5/32\n"
				if _, uErr := h.cs.LaboratoryV1alpha1().LabGroupClients("team-cli").UpdateStatus(ctx, c, metav1.UpdateOptions{}); uErr == nil {
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	out, err := h.CreateLabGroupClient(ctx, &protobuf.LabGroupClient{Namespace: "team-cli", Name: "alice"})
	<-done
	if err != nil {
		t.Fatalf("CreateLabGroupClient: %v", err)
	}

	cr, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-cli").Get(ctx, "alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if cr.Spec.PublicKey == "" {
		t.Errorf("expected a generated public key on the CR")
	}
	// The private key never touches the cluster: the CR config still holds the
	// placeholder, but the returned config has the real key substituted in.
	if !strings.Contains(cr.Status.Config, names.WGPrivateKeyPlaceholder) {
		t.Errorf("CR config should keep the placeholder: %q", cr.Status.Config)
	}
	if strings.Contains(out.Status.Config, names.WGPrivateKeyPlaceholder) {
		t.Errorf("returned config still has the placeholder: %q", out.Status.Config)
	}
	if !strings.Contains(out.Status.Config, "PrivateKey = ") {
		t.Errorf("returned config missing PrivateKey line: %q", out.Status.Config)
	}
}

func TestGetLabGroupClientStatistics(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	mustNamespace(t, k8s, "team-stats")

	lgc := &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "carol", Namespace: "team-stats"}}
	lgc.Spec.PublicKey = "pk"
	created, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-stats").Create(ctx, lgc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created.Status.AssignedIP = "10.8.0.5/32"
	created.Status.Config = "cfg"
	created.Status.Statistics = laboratoryv1alpha1.LabGroupClientStatistics{
		LastHandshake: metav1.Unix(1700000000, 0),
		RxBytes:       1234,
		TxBytes:       5678,
	}
	if _, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-stats").UpdateStatus(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update status: %v", err)
	}

	got, err := h.GetLabGroupClient(ctx, &protobuf.NamespacedIDRequest{Namespace: "team-stats", Name: "carol"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Config != "cfg" {
		t.Errorf("config not surfaced: %q", got.Status.Config)
	}
	if got.Status.Statistics == nil || got.Status.Statistics.RxBytes != 1234 || got.Status.Statistics.TxBytes != 5678 {
		t.Errorf("stats wrong: %+v", got.Status.Statistics)
	}
	if got.Status.Statistics.LastHandshakeUnix != 1700000000 {
		t.Errorf("last handshake wrong: %+v", got.Status.Statistics)
	}
}

func TestListAndDeleteLabGroupClient(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	mustNamespace(t, k8s, "team-cli-list")

	lgc := &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "team-cli-list"}}
	lgc.Spec.PublicKey = "pk2"
	if _, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-cli-list").Create(ctx, lgc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := h.ListLabGroupClients(ctx, &protobuf.NamespaceRequest{Namespace: "team-cli-list"})
	if err != nil {
		t.Fatalf("ListLabGroupClients: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "bob" {
		t.Fatalf("unexpected list: %+v", list.Items)
	}

	if _, err := h.DeleteLabGroupClient(ctx, &protobuf.NamespacedIDRequest{Namespace: "team-cli-list", Name: "bob"}); err != nil {
		t.Fatalf("DeleteLabGroupClient: %v", err)
	}
	if _, err := h.GetLabGroupClient(ctx, &protobuf.NamespacedIDRequest{Namespace: "team-cli-list", Name: "bob"}); err == nil {
		t.Fatalf("expected error getting deleted client, got nil")
	}
}
