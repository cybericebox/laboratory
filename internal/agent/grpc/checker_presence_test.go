package grpc

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// features.checks.enabled is true only while the option is on and the checker component is there.
func TestChecksFeatureNeedsTheCheckerComponent(t *testing.T) {
	ctx := context.Background()
	h, k8s := contractHandler(t, Features{ChecksEnabled: true, CheckerRequired: true})
	if h.checksFeature().Enabled {
		t.Fatal("the option is on but there is no checker DaemonSet: checks must be reported unavailable")
	}
	checks := &protobuf.LabChecks{Items: []*protobuf.Check{fileCheck("cfg", "web")}}
	res := createContractLab(t, h, &protobuf.LabVariant{SpecJson: contractSpecJSON(t, nil, "web"), Checks: checks}, &protobuf.LabItem{})
	invalidArg(t, res, nil, "checker is not installed")

	h2, k8s2 := contractHandler(t, Features{ChecksEnabled: true, CheckerRequired: true})
	if _, err := k8s2.AppsV1().DaemonSets(names.SystemNamespace).Create(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: names.CheckerDaemonSet, Namespace: names.SystemNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if !h2.checksFeature().Enabled {
		t.Fatal("the checker DaemonSet exists: checks are available")
	}
	res = createContractLab(t, h2, &protobuf.LabVariant{SpecJson: contractSpecJSON(t, nil, "web"), Checks: checks}, &protobuf.LabItem{})
	wantStates(t, res, nil, stCreated)

	// The answer is kept for a while: the DaemonSet going away is noticed after the TTL, not on every call.
	if err := k8s2.AppsV1().DaemonSets(names.SystemNamespace).Delete(ctx, names.CheckerDaemonSet, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !h2.checksFeature().Enabled {
		t.Fatal("the answer must be cached")
	}
	h2.checker.at = h2.checker.at.Add(-2 * checkerTTL)
	if h2.checksFeature().Enabled {
		t.Fatal("the checker is gone: checks must be reported unavailable after the cache expires")
	}
	_ = k8s
	// With the option off nothing is asked and nothing is available.
	off, _ := contractHandler(t, Features{CheckerRequired: true})
	if off.checksFeature().Enabled {
		t.Fatal("option off")
	}
}
