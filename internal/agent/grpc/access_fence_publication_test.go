package grpc

import (
	"context"
	"errors"
	"google.golang.org/protobuf/proto"
	"reflect"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	versionedfake "github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func accessFencePublicationRig() (*Handler, *versionedfake.Clientset, *protobuf.LabGroupAccessPolicy) {
	g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "group", UID: "group-uid", Labels: map[string]string{"event": "e"}}, Status: lab.LabGroupStatus{Namespace: "group-ns"}}
	cs := versionedfake.NewSimpleClientset(g)
	h := NewHandler(cs, kfake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "group-ns"}}), nil)
	p := &protobuf.LabGroupAccessPolicy{LabGroupName: "group", OperationId: "acl-current", DesiredRevision: 3, ExpectedGroupUid: string(g.UID), Rules: []*protobuf.LabGroupAccessRule{{Action: protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW}}}
	return h, cs, p
}

func accessFenceState(t *testing.T, h *Handler, p *protobuf.LabGroupAccessPolicy) protobuf.ItemState {
	t.Helper()
	res, err := h.SetLabGroupAccess(context.Background(), &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{p}})
	if err != nil || len(res.Results) != 1 {
		t.Fatalf("exported setter: %v %v", res, err)
	}
	return res.Results[0].State
}

func TestAccessFencePublicationMonotonicIdempotentAndLegacy(t *testing.T) {
	h, cs, p := accessFencePublicationRig()
	legacy := &protobuf.LabGroupAccessPolicy{LabGroupName: p.LabGroupName, Rules: p.Rules}
	if accessFenceState(t, h, legacy) != stCreated || accessFenceState(t, h, legacy) != stExists {
		t.Fatal("legacy contract changed")
	}
	if accessFenceState(t, h, p) != stUpdated {
		t.Fatal("unchanged rules did not publish fence")
	}
	if accessFenceState(t, h, p) != stExists {
		t.Fatal("exact fence tuple not idempotent")
	}
	for _, change := range []func(*protobuf.LabGroupAccessPolicy){
		func(p *protobuf.LabGroupAccessPolicy) { p.DesiredRevision = 2 },
		func(p *protobuf.LabGroupAccessPolicy) { p.OperationId = "conflict" },
		func(p *protobuf.LabGroupAccessPolicy) {
			p.Rules = []*protobuf.LabGroupAccessRule{{Action: protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_DENY}}
		},
		func(p *protobuf.LabGroupAccessPolicy) {
			p.OperationId = ""
			p.DesiredRevision = 0
			p.ExpectedGroupUid = ""
		},
		func(p *protobuf.LabGroupAccessPolicy) { p.PolicyUid = "foreign" },
		func(p *protobuf.LabGroupAccessPolicy) {
			p.OperationId = "new"
			p.DesiredRevision = 4
			p.Generation = 99
		},
	} {
		candidate := proto.Clone(p).(*protobuf.LabGroupAccessPolicy)
		change(candidate)
		if accessFenceState(t, h, candidate) != stFailed {
			t.Fatalf("stale/conflicting/legacy request accepted: %+v", candidate)
		}
	}
	stored, _ := cs.LaboratoryV1alpha1().LabGroupAccessPolicies("group-ns").Get(context.Background(), names.LabGroupAccessPolicyName, metav1.GetOptions{})
	if stored.Spec.OperationID != p.OperationId || stored.Spec.Revision != 3 || stored.Spec.Rules[0].Action != lab.LabGroupAccessAllow {
		t.Fatal("refusal modified current fence")
	}
	p.OperationId = "acl-next"
	p.DesiredRevision = 4
	if accessFenceState(t, h, p) != stUpdated {
		t.Fatal("newer explicit policy refused")
	}
}

func TestAccessFencePublicationPartialAndWrongGroup(t *testing.T) {
	for name, change := range map[string]func(*protobuf.LabGroupAccessPolicy){
		"missing op":                       func(p *protobuf.LabGroupAccessPolicy) { p.OperationId = "" },
		"missing revision":                 func(p *protobuf.LabGroupAccessPolicy) { p.DesiredRevision = 0 },
		"negative revision":                func(p *protobuf.LabGroupAccessPolicy) { p.DesiredRevision = -1 },
		"missing group uid":                func(p *protobuf.LabGroupAccessPolicy) { p.ExpectedGroupUid = "" },
		"wrong group uid":                  func(p *protobuf.LabGroupAccessPolicy) { p.ExpectedGroupUid = "wrong" },
		"missing policy uid target":        func(p *protobuf.LabGroupAccessPolicy) { p.PolicyUid = "expected-policy" },
		"missing policy generation target": func(p *protobuf.LabGroupAccessPolicy) { p.Generation = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			h, cs, p := accessFencePublicationRig()
			change(p)
			res, err := h.SetLabGroupAccess(context.Background(), &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{p}})
			if err == nil && (len(res.Results) != 1 || res.Results[0].State != stFailed) {
				t.Fatalf("incomplete/foreign authority accepted: %v", res)
			}
			if _, err := cs.LaboratoryV1alpha1().LabGroupAccessPolicies("group-ns").Get(context.Background(), names.LabGroupAccessPolicyName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatal("invalid request created a policy")
			}
		})
	}
}

func TestAccessFencePublicationRevalidatesGroupAndPolicyConflict(t *testing.T) {
	for _, boundary := range []string{"group replace on policy get", "group retirement on conflict", "policy superseded on conflict", "foreign tenant on conflict", "create collision"} {
		t.Run(boundary, func(t *testing.T) {
			h, cs, p := accessFencePublicationRig()
			if boundary != "create collision" && boundary != "group replace on policy get" {
				stored := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "group-ns", UID: "policy-uid", Generation: 2, ResourceVersion: "2"}, Spec: lab.LabGroupAccessPolicySpec{OperationID: "acl-old", Revision: 2, ExpectedGroupUID: "group-uid", Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessDeny}}}}
				if _, err := cs.LaboratoryV1alpha1().LabGroupAccessPolicies("group-ns").Create(context.Background(), stored, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			changed := false
			verb := "update"
			if boundary == "group replace on policy get" {
				verb = "get"
			}
			if boundary == "create collision" {
				verb = "create"
			}
			cs.PrependReactor(verb, "labgroupaccesspolicies", func(a ktesting.Action) (bool, runtime.Object, error) {
				if changed {
					return false, nil, nil
				}
				changed = true
				if boundary == "create collision" {
					obj := a.(ktesting.CreateAction).GetObject().DeepCopyObject()
					if err := cs.Tracker().Create(lab.SchemeGroupVersion.WithResource("labgroupaccesspolicies"), obj, "group-ns"); err != nil {
						return true, nil, err
					}
					return true, nil, apierrors.NewAlreadyExists(lab.Resource("labgroupaccesspolicies"), names.LabGroupAccessPolicyName)
				}
				if boundary == "policy superseded on conflict" {
					obj, err := cs.Tracker().Get(lab.SchemeGroupVersion.WithResource("labgroupaccesspolicies"), "group-ns", names.LabGroupAccessPolicyName)
					if err != nil {
						return true, nil, err
					}
					cur := obj.(*lab.LabGroupAccessPolicy)
					cur.Spec.OperationID = "newer"
					cur.Spec.Revision = 4
					if err := cs.Tracker().Update(lab.SchemeGroupVersion.WithResource("labgroupaccesspolicies"), cur, "group-ns"); err != nil {
						return true, nil, err
					}
				} else {
					obj, err := cs.Tracker().Get(lab.SchemeGroupVersion.WithResource("labgroups"), "", "group")
					if err != nil {
						return true, nil, err
					}
					g := obj.(*lab.LabGroup)
					switch boundary {
					case "group replace on policy get":
						g.UID = "replacement"
					case "group retirement on conflict":
						g.Annotations = map[string]string{names.AnnotationLifecycleRetirement: "retiring"}
					case "foreign tenant on conflict":
						g.Labels[names.LabelTenant] = "foreign"
					}
					if err := cs.Tracker().Update(lab.SchemeGroupVersion.WithResource("labgroups"), g, ""); err != nil {
						return true, nil, err
					}
				}
				if verb == "get" {
					return false, nil, nil
				}
				return true, nil, apierrors.NewConflict(lab.Resource("labgroupaccesspolicies"), names.LabGroupAccessPolicyName, errors.New("injected write race"))
			})
			state := accessFenceState(t, h, p)
			if boundary == "create collision" {
				if state != stExists {
					t.Fatalf("same tuple create race not idempotent: %v", state)
				}
			} else if state != stFailed && state != stNotFound {
				t.Fatalf("race bypassed authority/revision fence: %v", state)
			}
		})
	}
}

func TestAccessFenceGenerationEnvtest(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k8s, "fence-generation", "fence-generation-ns", nil)
	g, err := h.getGroup(ctx, "fence-generation")
	if err != nil {
		t.Fatal(err)
	}
	p := &protobuf.LabGroupAccessPolicy{LabGroupName: "fence-generation", Rules: []*protobuf.LabGroupAccessRule{{Action: protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW}}}
	if accessFenceState(t, h, p) != stCreated {
		t.Fatal("legacy seed failed")
	}
	before, _ := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(g.Status.Namespace).Get(ctx, names.LabGroupAccessPolicyName, metav1.GetOptions{})
	p.OperationId, p.DesiredRevision, p.ExpectedGroupUid, p.PolicyUid, p.Generation = "current", 3, string(g.UID), string(before.UID), before.Generation
	if accessFenceState(t, h, p) != stUpdated {
		t.Fatal("unchanged rules did not persist fence")
	}
	after, _ := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(g.Status.Namespace).Get(ctx, names.LabGroupAccessPolicyName, metav1.GetOptions{})
	if after.Generation <= before.Generation || !reflect.DeepEqual(before.Spec.Rules, after.Spec.Rules) || after.Spec.OperationID != p.OperationId {
		t.Fatalf("real CRD generation did not advance with fence only: before=%+v after=%+v", before, after)
	}
	if accessFenceState(t, h, p) != stExists {
		t.Fatal("same tuple retry with original read generation must be idempotent")
	}
	again, _ := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(g.Status.Namespace).Get(ctx, names.LabGroupAccessPolicyName, metav1.GetOptions{})
	if again.Generation != after.Generation {
		t.Fatal("exact retry advanced generation")
	}
}

func TestAccessFencePublicationExportedSetterStoresCurrentTuple(t *testing.T) {
	h, cs, p := accessFencePublicationRig()
	res, err := h.SetLabGroupAccess(context.Background(), &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{p}})
	if err != nil || len(res.Results) != 1 || res.Results[0].State != stCreated {
		t.Fatalf("exported setter: %v %v", res, err)
	}
	stored, err := cs.LaboratoryV1alpha1().LabGroupAccessPolicies("group-ns").Get(context.Background(), names.LabGroupAccessPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := accessPolicyToProto(stored, "group")
	if stored.Spec.OperationID != p.OperationId || stored.Spec.Revision != p.DesiredRevision || stored.Spec.ExpectedGroupUID != p.ExpectedGroupUid || got.OperationId != p.OperationId || got.DesiredRevision != p.DesiredRevision || got.ExpectedGroupUid != p.ExpectedGroupUid {
		t.Fatalf("setter dropped requested ACL fence before VPN/SDK projection: spec=%+v projected=%v", stored.Spec, got)
	}
}
