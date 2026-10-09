//go:build linux

package nodeagent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Exercise the normal producer and API CAS; observations are made by native
// adapters, not by a seeded Released scope report.
type publicationReader struct{ client.Client }

func (r publicationReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if devices, ok := list.(*lab.DeviceList); ok {
		*devices = lab.DeviceList{} // isolate the Lab publication from independent Device writes
		return nil
	}
	return r.Client.List(ctx, list, options...)
}

type publicationClient struct {
	client.Client
	t       *testing.T
	before  func(context.Context, *lab.Lab)
	patches int
}

func (c *publicationClient) Status() client.SubResourceWriter {
	return publicationStatus{SubResourceWriter: c.Client.Status(), parent: c}
}

type publicationStatus struct {
	client.SubResourceWriter
	parent *publicationClient
}

func (w publicationStatus) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
	if parent, ok := object.(*lab.Lab); ok {
		w.parent.patches++
		if w.parent.before != nil {
			before := w.parent.before
			w.parent.before = nil
			var live lab.Lab
			if err := w.parent.Client.Get(ctx, client.ObjectKeyFromObject(parent), &live); err != nil {
				w.parent.t.Fatal(err)
			}
			before(ctx, &live)
		}
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

func labPublicationFixture(t *testing.T) (*LifecycleReporter, *publicationClient, *lab.Lab, lab.OwnedRuntimeIdentity) {
	t.Helper()
	o, id, parent, device := persistentHistoricalNeverFixture(t)
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	parent.ResourceVersion, device.ResourceVersion = "", ""
	parent.Status.ScopeReports = []lab.OwnedRuntimeReport{{Identity: id, RuntimeState: "Unknown", Error: "pre-capture observation"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&lab.Lab{}, &lab.Device{}).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(object client.Object) []string { return []string{object.(*corev1.Pod).Spec.NodeName} }).
		WithObjects(parent, device).Build()
	o.Reader = c
	writes := &publicationClient{Client: c, t: t}
	return &LifecycleReporter{Client: writes, Reader: publicationReader{c}, Observer: o}, writes, parent, id
}

func TestLabScopePublicationConcurrentStatusPreservesNativeObservation(t *testing.T) {
	r, c, parent, id := labPublicationFixture(t)
	var concurrent lab.LabStatus
	c.before = func(ctx context.Context, live *lab.Lab) {
		live.Status.Lifecycle.ObservedState = "Unknown"
		live.Status.Lifecycle.Reason = "WaitingForNativeRuntimeAndFabricObservation"
		at := metav1.Now()
		live.Status.Resources = &lab.RuntimeAllocation{RuntimeState: "Unknown", ObservedAt: &at, SnapshotQuotaBytes: 100}
		foreign := id
		foreign.NodeName, foreign.NodeBootID = "other-node", "other-boot"
		live.Status.ScopeReports = append(live.Status.ScopeReports, lab.OwnedRuntimeReport{Identity: foreign, RuntimeState: "Unknown", Error: "other-node debt"})
		if err := c.Client.Status().Update(ctx, live); err != nil {
			t.Fatal(err)
		}
		concurrent = *live.Status.DeepCopy()
	}
	if err := r.sync(context.Background()); err != nil {
		t.Fatalf("genuine native scope observation lost to concurrent status version: %v", err)
	}
	var got lab.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Status.Lifecycle, concurrent.Lifecycle) || !reflect.DeepEqual(got.Status.Resources, concurrent.Resources) {
		t.Fatalf("publication overwrote concurrent controller status")
	}
	if len(got.Status.ScopeReports) != 2 || got.Status.ScopeReports[0].Error != "other-node debt" {
		t.Fatalf("publication erased another node's unresolved scope: %+v", got.Status.ScopeReports)
	}
	produced := got.Status.ScopeReports[1]
	if produced.RuntimeState != "Released" || !sameDeclaredNativeScope(produced.Identity, id) || produced.RuntimeAbsentAt == nil || produced.CgroupAbsentAt == nil || produced.AttachmentsAbsentAt == nil {
		t.Fatalf("genuine native release was not published: %+v", produced)
	}
}

func TestLabScopePublicationRejectsChangedAuthorityDuringConflict(t *testing.T) {
	changes := map[string]func(*lab.Lab){
		"replacement-lab":           func(l *lab.Lab) { l.UID = "replacement" },
		"new-start":                 func(l *lab.Lab) { l.Spec.Lifecycle.DesiredState = "Running" },
		"operation":                 func(l *lab.Lab) { l.Spec.Lifecycle.OperationID = "replacement" },
		"revision":                  func(l *lab.Lab) { l.Spec.Lifecycle.Revision++ },
		"generation":                func(l *lab.Lab) { l.Generation++ },
		"missing-barrier":           func(l *lab.Lab) { l.Status.Lifecycle = nil },
		"barrier-cleared":           func(l *lab.Lab) { l.Status.Lifecycle.SnapshotComplete = false },
		"barrier-owner":             func(l *lab.Lab) { l.Status.Lifecycle.LabUID = "foreign" },
		"barrier-operation":         func(l *lab.Lab) { l.Status.Lifecycle.OperationID = "replacement" },
		"barrier-revision":          func(l *lab.Lab) { l.Status.Lifecycle.Revision++ },
		"barrier-generation":        func(l *lab.Lab) { l.Status.Lifecycle.ObservedGeneration++ },
		"declaration-replaced":      func(l *lab.Lab) { l.Status.ScopeInventory[0].ScopeUID = "replacement" },
		"declaration-owner":         func(l *lab.Lab) { l.Status.ScopeInventory[0].OwnerUID = "foreign" },
		"declaration-node":          func(l *lab.Lab) { l.Status.ScopeInventory[0].NodeName = "foreign" },
		"declaration-boot":          func(l *lab.Lab) { l.Status.ScopeInventory[0].NodeBootID = "replacement" },
		"declaration-physical-debt": func(l *lab.Lab) { l.Status.ScopeInventory[0].ContainerIDs = []string{"new-debt"} },
		"new-same-node-declaration": func(l *lab.Lab) {
			added := *l.Status.ScopeInventory[0].DeepCopy()
			added.ScopeUID = "new-device"
			l.Status.ScopeInventory = append(l.Status.ScopeInventory, added)
		},
		"retirement-nonce": func(l *lab.Lab) {
			l.Annotations = map[string]string{names.AnnotationLifecycleRetirement: "new-retirement-challenge"}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			r, c, parent, _ := labPublicationFixture(t)
			var concurrent *lab.Lab
			c.before = func(ctx context.Context, live *lab.Lab) {
				change(live)
				status := live.Status.DeepCopy()
				if err := c.Client.Update(ctx, live); err != nil {
					t.Fatal(err)
				}
				live.Status = *status
				if err := c.Client.Status().Update(ctx, live); err != nil {
					t.Fatal(err)
				}
				concurrent = live.DeepCopy()
			}
			if err := r.sync(context.Background()); err == nil {
				t.Fatal("stale native observation adopted after authority changed")
			}
			var got lab.Lab
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &got); err != nil {
				t.Fatal(err)
			}
			if concurrent == nil || !reflect.DeepEqual(got.Status, concurrent.Status) || !reflect.DeepEqual(got.Spec, concurrent.Spec) || got.UID != concurrent.UID || !reflect.DeepEqual(got.Annotations, concurrent.Annotations) || c.patches != 1 {
				t.Fatal("stale publication mutated newer authority or retried without revalidation")
			}
		})
	}
}

func TestLabScopePublicationRetainsCachedHistoricalCertificateAfterRestore(t *testing.T) {
	r, c, parent, id := labPublicationFixture(t)
	produced := r.Observer.ObserveScope(context.Background(), id)
	if produced.RuntimeState != "Released" {
		t.Fatalf("native fixture failed to commit historical release: %+v", produced)
	}
	if err := r.Observer.readRecord("scope-fabric-released", produced.Identity, &produced); err != nil {
		t.Fatal(err)
	}
	var live lab.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &live); err != nil {
		t.Fatal(err)
	}
	// The retained declaration is now exactly the committed native identity;
	// this is the existing ObserveScope durable-certificate fast path.
	live.Status.ScopeInventory[0] = produced.Identity
	live.Generation++
	live.Spec.Lifecycle.OperationID, live.Spec.Lifecycle.Revision = "post-restore-stop", 9
	live.Status.Lifecycle.OperationID, live.Status.Lifecycle.Revision, live.Status.Lifecycle.ObservedGeneration = "post-restore-stop", 9, live.Generation
	status := live.Status.DeepCopy()
	if err := c.Update(context.Background(), &live); err != nil {
		t.Fatal(err)
	}
	live.Status = *status
	if err := c.Status().Update(context.Background(), &live); err != nil {
		t.Fatal(err)
	}
	var device lab.Device
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "d"}, &device); err != nil {
		t.Fatal(err)
	}
	device.Status.State.Epoch, device.Status.State.Incarnation = 1, 2
	device.Status.State.Capture = nil
	if err := c.Status().Update(context.Background(), &device); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Observer.scopeCurrent(context.Background(), produced.Identity); err == nil {
		t.Fatal("fixture still qualifies through live persistent proof")
	}
	if err := r.sync(context.Background()); err != nil {
		t.Fatalf("cached committed historical authority lost after restore: %v", err)
	}
	var got lab.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.ScopeReports) != 1 || !reflect.DeepEqual(got.Status.ScopeReports[0], produced) {
		t.Fatalf("historical native certificate changed during publication: %+v", got.Status.ScopeReports)
	}
}

func TestLabScopePublicationPreservesHistoryAndNewerUnknown(t *testing.T) {
	r, c, parent, id := labPublicationFixture(t)
	c.before = func(ctx context.Context, live *lab.Lab) {
		history := id
		history.OperationID, history.Revision, history.Generation = "older-operation", 1, 1
		newer := metav1.NewTime(time.Now().Add(time.Minute))
		live.Status.ScopeReports = []lab.OwnedRuntimeReport{
			{Identity: history, RuntimeState: "Released", Error: "historical row retained"},
			{Identity: id, RuntimeState: "Unknown", ObservedAt: &newer, Error: "newer unresolved observation"},
		}
		if err := c.Client.Status().Update(ctx, live); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got lab.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.ScopeReports) != 2 || got.Status.ScopeReports[0].Error != "historical row retained" || got.Status.ScopeReports[1].Error != "newer unresolved observation" || got.Status.ScopeReports[1].RuntimeState != "Unknown" {
		t.Fatalf("publication erased history or overwrote newer native debt: %+v", got.Status.ScopeReports)
	}
}

func TestLabScopePublicationConflictBudgetAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded-conflicts", true: "cancelled"}[cancel], func(t *testing.T) {
			r, c, parent, _ := labPublicationFixture(t)
			flowReads, scannedBeforeCAS := 0, 0
			r.Observer.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
				flowReads++
				return nil, nil
			}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			var churn func(context.Context, *lab.Lab)
			churn = func(ctx context.Context, live *lab.Lab) {
				if scannedBeforeCAS == 0 {
					scannedBeforeCAS = flowReads
				}
				live.Status.Lifecycle.Reason += "concurrent-write"
				if err := c.Client.Status().Update(ctx, live); err != nil {
					t.Fatal(err)
				}
				if cancel {
					stop()
				} else {
					c.before = churn
				}
			}
			c.before = churn
			if err := r.sync(ctx); err == nil {
				t.Fatal("unbounded conflicts or cancellation were hidden")
			}
			want := 3
			if cancel {
				want = 1
			}
			if c.patches != want {
				t.Fatalf("publication attempts=%d, want %d", c.patches, want)
			}
			if scannedBeforeCAS == 0 || flowReads != scannedBeforeCAS {
				t.Fatal("conflict retries reran or bypassed genuine native scans")
			}
			var got lab.Lab
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status.ScopeReports[0].RuntimeState != "Unknown" {
				t.Fatal("failed publication credited release")
			}
		})
	}
}

func TestLabScopePublicationRetirementChallengeMustRemainExact(t *testing.T) {
	r, c, parent, id := labPublicationFixture(t)
	var live lab.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &live); err != nil {
		t.Fatal(err)
	}
	intent := lab.LifecycleRetirementIntent{ExpectedUID: string(live.UID), StopOperationID: id.OperationID, StopRevision: id.Revision, Generation: live.Generation, OperationID: "retire", Revision: 9, RequestedAt: metav1.Now()}
	raw, _ := json.Marshal(intent)
	live.Annotations = map[string]string{names.AnnotationLifecycleRetirement: string(raw)}
	if err := c.Update(context.Background(), &live); err != nil {
		t.Fatal(err)
	}
	c.before = func(ctx context.Context, live *lab.Lab) {
		intent.OperationID, intent.Revision = "new-retirement", 10
		raw, _ := json.Marshal(intent)
		live.Annotations[names.AnnotationLifecycleRetirement] = string(raw)
		if err := c.Client.Update(ctx, live); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.sync(context.Background()); err == nil {
		t.Fatal("old physical scan acknowledged a new retirement challenge")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &live); err != nil {
		t.Fatal(err)
	}
	if live.Status.ScopeReports[0].RetirementOperationID != "" || c.patches != 1 {
		t.Fatal("stale retirement ACK was published")
	}
}

func TestLabScopePublicationUnknownDebtStaysUnknown(t *testing.T) {
	r, c, parent, _ := labPublicationFixture(t)
	var d lab.Device
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "d"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Status.State.Capture.Committed = false
	if err := c.Status().Update(context.Background(), &d); err != nil {
		t.Fatal(err)
	}
	if err := r.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got lab.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.ScopeReports) != 1 || got.Status.ScopeReports[0].RuntimeState != "Unknown" || got.Status.ScopeReports[0].AttachmentsAbsentAt != nil {
		t.Fatalf("publication converted unresolved native debt into release: %+v", got.Status.ScopeReports)
	}
}
