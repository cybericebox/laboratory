package nodeagent

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics of the wiring of device pods on a node. The labels are fixed names (a lock, the function that holds it, a result), never a pod.

var (
	setupNetworksSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "laboratory_nodeagent_setup_networks_seconds",
		Help:    "Duration of one SetupNetworks call (one pod's CNI ADD), the wait for the pod in the cache and for the wiring lock included. result: ok or error.",
		Buckets: wiringBuckets,
	}, []string{"result"})
	wireInterfaceSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "laboratory_nodeagent_wire_interface_seconds",
		Help:    "Time to wire one interface of a pod: the owner check, the wait for the wiring lock and the wiring itself. result: ok or error.",
		Buckets: wiringBuckets,
	}, []string{"result"})
	lockWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "laboratory_nodeagent_lock_wait_seconds",
		Help:    "Time waiting to take a node-wide lock (lock: veth, the wiring lock). 0 when it was free.",
		Buckets: wiringBuckets,
	}, []string{"lock"})
	lockHoldSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "laboratory_nodeagent_lock_hold_seconds",
		Help:    "Time a node-wide lock was held, by the function that held it.",
		Buckets: wiringBuckets,
	}, []string{"lock", "holder"})
)

// wiringBuckets go from a millisecond to a minute.
var wiringBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

func init() {
	metrics.Registry.MustRegister(setupNetworksSeconds, wireInterfaceSeconds, lockWaitSeconds, lockHoldSeconds)
}

func resultOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func since(t time.Time) float64 { return time.Since(t).Seconds() }
