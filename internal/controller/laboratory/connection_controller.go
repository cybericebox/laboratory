package laboratory

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// ConnectionReconciler reconciles a Connection object.
// The operator role is minimal: node-agent writes port status;
// this reconciler aggregates port readiness into Connection.status.ready.
type ConnectionReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=connections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=connections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=connections/finalizers,verbs=update

func (r *ConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var conn laboratoryv1alpha1.Connection
	if err := r.Get(ctx, req.NamespacedName, &conn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !conn.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&conn, names.FinalizerOVSCleanup) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}

	stopped, err := (&DeviceReconciler{Client: r.Client}).deviceStopped(ctx, &laboratoryv1alpha1.Device{ObjectMeta: conn.ObjectMeta, Spec: laboratoryv1alpha1.DeviceSpec{LabRef: conn.Spec.LabRef}})
	if err != nil {
		return ctrl.Result{}, err
	}
	allConnected := len(conn.Status.Ports) == len(conn.Spec.Endpoints)
	for _, p := range conn.Status.Ports {
		if !p.Connected {
			allConnected = false
			break
		}
	}

	if stopped {
		allConnected = false
	}
	if conn.Status.Ready != allConnected {
		conn.Status.Ready = allConnected
		if allConnected {
			labstatus.SetReady(
				&conn.Status.Conditions, conn.Generation, true, labstatus.ReasonReady,
				"all endpoint ports connected",
			)
			r.Recorder.Event(&conn, corev1.EventTypeNormal, labstatus.ReasonReady, "connection ready")
		} else {
			labstatus.SetReady(
				&conn.Status.Conditions, conn.Generation, false, labstatus.ReasonWaitingForPort,
				"waiting for all endpoint ports to connect",
			)
		}
		return ctrl.Result{}, r.Status().Update(ctx, &conn)
	}

	return ctrl.Result{}, nil
}

func (r *ConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Connection{}).
		Watches(&laboratoryv1alpha1.Lab{}, handler.EnqueueRequestsFromMapFunc(r.connectionsForLab)).
		Complete(reconcileutil.Quiet(r))
}

func (r *ConnectionReconciler) connectionsForLab(ctx context.Context, obj client.Object) []reconcile.Request {
	var cs laboratoryv1alpha1.ConnectionList
	if err := r.List(ctx, &cs, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, c := range cs.Items {
		if c.Spec.LabRef == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&c)})
		}
	}
	return out
}
