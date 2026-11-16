package grpc

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics of creating labs. No lab, group or tenant name is a label.
var (
	createLabSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "laboratory_agent_create_lab_seconds",
		Help:    "Time to create one lab of a CreateLabs call (the wait for its group's admission lock included). state: CREATED, EXISTS, UPDATED, or error.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
	}, []string{"state"})
	groupLockWaitSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "laboratory_agent_group_lock_wait_seconds",
		Help:    "Time a lifecycle or lab creation call waited for the lock of its group (one admission slot per group).",
		Buckets: []float64{0.0005, 0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	})
)

func init() { metrics.Registry.MustRegister(createLabSeconds, groupLockWaitSeconds) }

func sinceSeconds(t time.Time) float64 { return time.Since(t).Seconds() }
