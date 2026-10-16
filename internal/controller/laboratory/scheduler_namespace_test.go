package laboratory

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// A group's runtime namespace can differ from its name. Ready group pods must
// complete scheduling (and recover a prior timeout) in that actual namespace.
func TestSchedulerObservesGroupPodsInTheirNamespace(t *testing.T) {
	for _, namespace := range []struct {
		name, recorded, actual string
	}{
		{"generated", "", laboratoryv1alpha1.LabGroupNamespace("team")},
		{"recorded", "lg-team-recorded", "lg-team-recorded"},
		{"legacy", "team", "team"},
	} {
		for _, scenario := range []struct {
			name  string
			state laboratoryv1alpha1.PodScheduleState
			ready bool
			want  laboratoryv1alpha1.PodScheduleState
		}{
			{"starting pod becomes started", laboratoryv1alpha1.PodStarting, true, laboratoryv1alpha1.PodStarted},
			{"failed pod recovers", laboratoryv1alpha1.PodFailed, true, laboratoryv1alpha1.PodStarted},
			{"missing pod times out", laboratoryv1alpha1.PodStarting, false, laboratoryv1alpha1.PodFailed},
		} {
			t.Run(namespace.name+"/"+scenario.name, func(t *testing.T) {
				f := newSchedFixture(t, schedCfg())
				dispatched := metav1.NewTime(f.now.Add(-6 * time.Minute))
				ps := laboratoryv1alpha1.PodSchedule{State: scenario.state, DispatchedAt: &dispatched}
				if scenario.state == laboratoryv1alpha1.PodFailed {
					ps.Failure = &laboratoryv1alpha1.PodFailure{Reason: laboratoryv1alpha1.FailureStartupTimeout, Message: "no pod was created", At: &dispatched}
				}
				f.create(&laboratoryv1alpha1.LabGroup{
					ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "uid-team"},
					Spec:       laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}},
					Status: laboratoryv1alpha1.LabGroupStatus{
						Namespace: namespace.recorded,
						Pods:      []laboratoryv1alpha1.NamedPodSchedule{{Name: names.ComponentGateway, PodSchedule: ps}},
					},
				})
				f.createPlain(&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: names.ComponentGateway, Namespace: namespace.actual},
					Spec:       appsv1.DeploymentSpec{Replicas: ptrInt32(1)},
				})
				if scenario.ready {
					pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
						Name: "gateway-ready", Namespace: namespace.actual,
						Labels: map[string]string{names.LabelComponent: names.ComponentGateway},
					}}
					f.createPlain(pod)
					pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
					if err := f.c.Status().Update(context.Background(), pod); err != nil {
						t.Fatal(err)
					}
				}

				f.tick()
				var group laboratoryv1alpha1.LabGroup
				if err := f.c.Get(context.Background(), types.NamespacedName{Name: "team"}, &group); err != nil {
					t.Fatal(err)
				}
				got := group.Status.Pods[0].PodSchedule
				if got.State != scenario.want {
					t.Fatalf("gateway schedule = %+v, want state %s", got, scenario.want)
				}
				if scenario.ready {
					if got.Failure != nil || got.StartedAt == nil {
						t.Fatalf("ready gateway retained a failure or lacks its start time: %+v", got)
					}
				} else if got.Failure == nil || got.Failure.Reason != laboratoryv1alpha1.FailureStartupTimeout || got.Failure.Message != "no pod was created" {
					t.Fatalf("missing gateway did not retain its startup timeout: %+v", got)
				}
			})
		}
	}
}

// One scheduler snapshot can contain many teams with distinct runtime
// namespaces. A missing gateway must fail without misclassifying Ready peers.
func TestSchedulerObservesGroupNamespaceBatch(t *testing.T) {
	const groupCount = 100
	f := newSchedFixture(t, schedCfg())
	dispatched := metav1.NewTime(f.now.Add(-6 * time.Minute))
	components := []string{names.ComponentVPN, names.ComponentGateway}
	createReadyPod := func(namespace, component string) {
		t.Helper()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: component + "-ready", Namespace: namespace,
			Labels: map[string]string{names.LabelComponent: component},
		}}
		f.createPlain(pod)
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		if err := f.c.Status().Update(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
	}
	for i := range groupCount {
		name := fmt.Sprintf("team-%03d", i)
		namespace := laboratoryv1alpha1.LabGroupNamespace(name)
		group := &laboratoryv1alpha1.LabGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
			Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: namespace},
		}
		for _, component := range components {
			group.Status.Pods = append(group.Status.Pods, laboratoryv1alpha1.NamedPodSchedule{
				Name: component, PodSchedule: laboratoryv1alpha1.PodSchedule{State: laboratoryv1alpha1.PodStarting, DispatchedAt: &dispatched},
			})
			f.createPlain(&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: component, Namespace: namespace},
				Spec:       appsv1.DeploymentSpec{Replicas: ptrInt32(1)},
			})
			if i != groupCount-1 || component != names.ComponentGateway {
				createReadyPod(namespace, component)
			}
		}
		f.create(group)
	}
	assertSchedules := func(missingGateway bool) {
		t.Helper()
		for i := range groupCount {
			name := fmt.Sprintf("team-%03d", i)
			var group laboratoryv1alpha1.LabGroup
			if err := f.c.Get(context.Background(), types.NamespacedName{Name: name}, &group); err != nil {
				t.Fatal(err)
			}
			if len(group.Status.Pods) != len(components) {
				t.Fatalf("%s has %d scheduling entries, want %d", name, len(group.Status.Pods), len(components))
			}
			for _, got := range group.Status.Pods {
				if missingGateway && i == groupCount-1 && got.Name == names.ComponentGateway {
					if got.State != laboratoryv1alpha1.PodFailed || got.Failure == nil || got.Failure.Reason != laboratoryv1alpha1.FailureStartupTimeout {
						t.Fatalf("%s/%s missing pod did not time out: %+v", name, got.Name, got)
					}
					continue
				}
				if got.State != laboratoryv1alpha1.PodStarted || got.Failure != nil || got.StartedAt == nil {
					t.Fatalf("%s/%s Ready pod was not observed: %+v", name, got.Name, got)
				}
			}
		}
	}

	f.tick()
	assertSchedules(true)
	createReadyPod(laboratoryv1alpha1.LabGroupNamespace("team-099"), names.ComponentGateway)
	f.tick()
	assertSchedules(false)
}
