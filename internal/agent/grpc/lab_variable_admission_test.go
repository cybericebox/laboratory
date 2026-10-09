package grpc

import (
	"context"
	"encoding/json"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	fakeversioned "github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/internal/names"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestVariableAdmissionFencesRetirementAndIdenticalRetry(t *testing.T) {
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "ns", UID: "uid", Generation: 7}}
	cs := fakeversioned.NewSimpleClientset(l)
	h := &Handler{cs: cs}
	ctx := context.Background()
	token := variableWriteToken(l, []string{"web"}, deviceVars{"web": {"FLAG": "private"}})
	if err := h.claimVariableWrite(ctx, l, token); err != nil {
		t.Fatal(err)
	}
	if err := h.claimVariableWrite(ctx, l, token); err != nil {
		t.Fatal("identical retry", err)
	}
	if err := h.claimVariableWrite(ctx, l, "different"); err == nil {
		t.Fatal("different concurrent write admitted")
	}
	current, _ := cs.LaboratoryV1alpha1().Labs("ns").Get(ctx, "lab", metav1.GetOptions{})
	if current.Annotations[names.AnnotationLabVariableAdmission] != token {
		t.Fatal("durable admission missing")
	}
	if err := h.finishVariableWrite(ctx, l, token); err != nil {
		t.Fatal(err)
	}
	current, _ = cs.LaboratoryV1alpha1().Labs("ns").Get(ctx, "lab", metav1.GetOptions{})
	in := lab.LifecycleRetirementIntent{ExpectedUID: "uid", StopOperationID: "stop", StopRevision: 2, Generation: 7, OperationID: "retire", Revision: 3, RequestedAt: metav1.Now()}
	raw, _ := json.Marshal(in)
	current.Annotations[names.AnnotationLifecycleRetirement] = string(raw)
	_, _ = cs.LaboratoryV1alpha1().Labs("ns").Update(ctx, current, metav1.UpdateOptions{})
	if err := h.claimVariableWrite(ctx, l, token); err == nil {
		t.Fatal("retired Lab write revived private values")
	}
}
