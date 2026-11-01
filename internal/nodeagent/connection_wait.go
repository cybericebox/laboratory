package nodeagent

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// A Connection that the node-agent cannot program yet says why, in the status, instead of requeueing in silence: the condition Wired is
// False with a fixed reason (a code, no paths) and a short message, True once the ports are programmed. One log line is written when the
// reason changes, not at every retry. (The condition Ready is the operator's: the ports are connected.)
const (
	// ConditionWired is the type of the node-agent's condition on a Connection.
	ConditionWired = "Wired"

	WaitEndpointNode      = "WaitingForEndpointNode"  // an endpoint device has no node yet (its pod is not placed or not started)
	WaitVNI               = "WaitingForVNI"           // the connection or the switch has no VNI yet
	WaitNetworkObject     = "WaitingForNetworkObject" // the LabVPN or LabGateway of the lab is not there yet
	WaitInvalidNetworkIdx = "InvalidNetworkIndex"     // the lab network index is 0 or out of 1..254: the pod has no interface for it
	WaitDevicePod         = "WaitingForDevicePod"     // the device's pod is not published yet
	WaitOVSPort           = "WaitingForOVSPort"       // the port of an endpoint does not exist on this node yet
	WaitPortOwner         = "WaitingForPortOwner"     // the port of an endpoint still belongs to the pod that was replaced (its new pod is being wired)
	WaitVNIRelease        = "WaitingForVNIRelease"    // deleting: the nodes have not acknowledged the release of the lab's VNI yet
	WaitCleanupBlocked    = "CleanupBlocked"          // deleting: a step of the cleanup cannot finish yet (the message says which error)
	ReasonProgrammed      = "Programmed"              // the ports are programmed
	wiredRetryOnWait      = 3 * time.Second
	wiredRetryOnWaitSlow  = 5 * time.Second
)

// markWired sets the condition Wired on the Connection and writes the status when it changed. It logs once when the reason changes.
func markWired(ctx context.Context, c client.Client, log logr.Logger, conn *laboratoryv1alpha1.Connection, wired bool, reason, message string) error {
	status := metav1.ConditionFalse
	if wired {
		status = metav1.ConditionTrue
	}
	old := meta.FindStatusCondition(conn.Status.Conditions, ConditionWired)
	if old != nil && old.Status == status && old.Reason == reason && old.Message == message && old.ObservedGeneration == conn.Generation {
		return nil
	}
	if old == nil || old.Reason != reason {
		log.Info("connection wiring", "connection", conn.Namespace+"/"+conn.Name, "wired", wired, "reason", reason, "message", message)
	}
	meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
		Type: ConditionWired, Status: status, ObservedGeneration: conn.Generation, Reason: reason, Message: message,
	})
	return c.Status().Update(ctx, conn)
}

// validLabIndex says n is a lab network index that the VPN and gateway pods have an interface for (lab1..lab254; 0 is not one).
func validLabIndex(n uint) bool { return n >= 1 && n <= maxLabIndex }

// waitMessage is the text of a reason, for the message of the condition.
func waitMessage(reason string) string {
	switch reason {
	case WaitNetworkObject:
		return "the LabVPN or LabGateway of the lab is not there yet"
	case WaitInvalidNetworkIdx:
		return "the lab network index is not in 1..254, so the pod has no interface for it"
	case WaitDevicePod:
		return "the pod of the device is not published yet"
	case WaitVNIRelease:
		return "the nodes have not acknowledged the release of the VNI of the lab yet (see status.scopeReports of the Lab)"
	case WaitOVSPort:
		return "the OVS port does not exist on this node yet"
	case WaitPortOwner:
		return "the port of the endpoint does not belong to its current pod yet (the pod was replaced and is being wired)"
	}
	return reason
}
