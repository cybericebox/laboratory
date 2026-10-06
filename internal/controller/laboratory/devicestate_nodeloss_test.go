package laboratory

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestForceDeleteLostPod(t *testing.T) {
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	scheme := pruneScheme(t)
	_ = corev1.AddToScheme(scheme)
	node := func(name string, ready corev1.ConditionStatus) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}}}
	}
	pod := func(nodeName string, terminatingSince *time.Time) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dev-1", Namespace: "ns", Finalizers: []string{"test/hold"}}, Spec: corev1.PodSpec{NodeName: nodeName}}
		if terminatingSince != nil {
			p.DeletionTimestamp = &metav1.Time{Time: *terminatingSince}
		}
		return p
	}
	ago := func(d time.Duration) *time.Time { x := t0.Add(-d); return &x }

	cases := []struct {
		name    string
		pod     *corev1.Pod
		nodes   []client.Object
		after   time.Duration
		deleted bool
	}{
		{"node NotReady, past the timeout", pod("n1", ago(6*time.Minute)), []client.Object{node("n1", corev1.ConditionUnknown)}, 0, true},
		{"node NotReady (False), past a custom timeout", pod("n1", ago(2*time.Minute)), []client.Object{node("n1", corev1.ConditionFalse)}, time.Minute, true},
		{"node gone", pod("n1", ago(6*time.Minute)), nil, 0, true},
		{"node NotReady, not yet", pod("n1", ago(4*time.Minute)), []client.Object{node("n1", corev1.ConditionUnknown)}, 0, false},
		{"node Ready: the pod is only slow", pod("n1", ago(time.Hour)), []client.Object{node("n1", corev1.ConditionTrue)}, 0, false},
		{"not terminating", pod("n1", nil), []client.Object{node("n1", corev1.ConditionUnknown)}, 0, false},
		{"never scheduled", pod("", ago(time.Hour)), nil, 0, false},
		{"disabled", pod("n1", ago(time.Hour)), []client.Object{node("n1", corev1.ConditionUnknown)}, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var grace *int64
			deletes := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(tc.nodes, tc.pod)...).
				WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					o := &client.DeleteOptions{}
					o.ApplyOptions(opts)
					grace = o.GracePeriodSeconds
					deletes++
					return nil
				}}).Build()
			r := &DeviceReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return t0 }, NodeLossForceDeleteAfter: tc.after}
			var cur corev1.Pod
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(tc.pod), &cur); err != nil {
				t.Fatal(err)
			}
			got, err := r.forceDeleteLostPod(context.Background(), &cur)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.deleted || (deletes == 1) != tc.deleted {
				t.Fatalf("deleted=%v (%d calls), want %v", got, deletes, tc.deleted)
			}
			if tc.deleted && (grace == nil || *grace != 0) {
				t.Errorf("the pod must be deleted with grace period 0, got %v", grace)
			}
		})
	}
}
