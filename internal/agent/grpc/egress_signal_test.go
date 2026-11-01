package grpc

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// The daemon is a client of the laboratory: what the egress monitor says about a lab (the condition EgressAttention) is for the cluster's
// administrator and must not show in what the agent tells the daemon.
func TestEgressAttentionDoesNotReachTheDaemon(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns"}}
	lab.Status.Phase = laboratoryv1alpha1.PhaseReady
	plain := labProjection(lab, true)
	flagged := lab.DeepCopy()
	meta.SetStatusCondition(&flagged.Status.Conditions, metav1.Condition{Type: "EgressAttention", Status: metav1.ConditionTrue, Reason: "EgressSignal", Message: "30 labs reach one host"})
	if !proto.Equal(plain, labProjection(flagged, true)) {
		t.Fatal("the EgressAttention condition must not change what the daemon is told about a lab")
	}
}
