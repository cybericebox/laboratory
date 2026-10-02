package grpc

import (
	"context"
	"github.com/google/go-containerregistry/pkg/authn"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"k8s.io/client-go/kubernetes"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Handler implements protobuf.LabManagerServer against the laboratory
// typed clientset (custom resources on the workload cluster) and, for
// resources whose data lives partly in core Secrets (e.g. LabGroupClient's
// wg.conf), the plain kubernetes clientset.
type Handler struct {
	protobuf.UnimplementedLabManagerServer
	cs  versioned.Interface
	k8s kubernetes.Interface
	// metrics is optional: nil when metrics-server is not installed. Live
	// resource-usage reporting then degrades to zero rather than failing.
	metrics metricsclient.Interface
	agentID string

	// monitor is the shared poller and journal behind every Monitoring stream.
	monOnce sync.Once
	mon     *monitor

	// statePersistence: the cluster allows devices with persistence (the chart switch).
	statePersistence bool

	// caCertFile/caKeyFile sign client certificates (Enroll, RenewCertificate).
	caCertFile, caKeyFile string
	certTTL               time.Duration
	clock                 func() time.Time
	// renewMinInterval is how often a tenant may renew its certificate (DefaultRenewMinInterval when zero); lastRenew
	// remembers the last renewal of each tenant.
	// Monitoring streams open per tenant (see MonitoringConfig.MaxStreamsPerTenant).
	monMu            sync.Mutex
	monStreams       map[string]int
	monMaxStreams    int
	renewMinInterval time.Duration
	renewMu          sync.Mutex
	lastRenew        map[string]time.Time

	// groupOverhead is what a LabGroup's own pods (VPN, gateway) request together.
	groupOverhead grouppods.Overhead

	// labSelector and labTolerations describe the nodes lab pods run on (percentage quotas).
	labSelector    map[string]string
	labTolerations []corev1.Toleration
	// nodeReserve is what the platform keeps free on the lab nodes (the scheduler's reserve), applied to the per-node room.
	nodeReserve nodecap.Reserve
	// roomCache keeps the largest placeable device for a few seconds: it is the same for every tenant.
	roomCache deviceRoomCache
	// errors is the laboratory's own error journal (see errorjournal.go); errJournal says where its events are.
	errors     *errorCollector
	errJournal ErrorJournalConfig
	// features are the platform's choices GetFeatures reports; featCache keeps each tenant's answer for a few seconds.
	features  Features
	featCache featuresCache
	// capCache keeps each tenant's capacity for a few seconds.
	capCache capacityCache

	// prewarm fills the image cache ahead of time; nil until SetPrewarm.
	prewarm *prewarmer

	// imageDeny are the "registry/repository-prefix" entries no tenant may use; imageCache
	// are the addresses of the platform registry and cache (tenants may not name them).
	imageDeny  []string
	imageCache []string

	// registryAddr is the platform registry (host:port) snapshot export reads from.
	registryAddr string
	registryAuth authn.Authenticator
}

// NewHandler builds a Handler backed by the given typed clientset, plain
// kubernetes clientset, and (optionally) a metrics clientset. A nil metrics
// client disables live usage reporting without erroring.
func NewHandler(cs versioned.Interface, k8s kubernetes.Interface, metrics metricsclient.Interface, agentID ...string) *Handler {
	id := "laboratory-agent"
	if len(agentID) > 0 && agentID[0] != "" {
		id = agentID[0]
	}
	return &Handler{cs: cs, k8s: k8s, metrics: metrics, agentID: id}
}

// Ping answers the backend's connection health probe (mTLS and CN allowlist
// have already been checked by the server interceptors).
func (h *Handler) Ping(context.Context, *protobuf.Empty) (*protobuf.Empty, error) {
	return &protobuf.Empty{}, nil
}

// SetStatePersistence tells the agent whether the cluster allows device state
// persistence; a topology that asks for it otherwise is refused.
func (h *Handler) SetStatePersistence(enabled bool) { h.statePersistence = enabled }

// SetImagePolicy sets the platform's side of the image policy: the repositories no tenant
// may use and the addresses of the platform registry and image cache. A tenant's own allow
// list (Tenant.spec.images.allow) narrows the rest.
func (h *Handler) SetImagePolicy(deny []string, cacheAddrs ...string) {
	h.imageDeny = deny
	h.imageCache = cacheAddrs
}

func isNotFound(err error) bool { return apierrors.IsNotFound(err) }
