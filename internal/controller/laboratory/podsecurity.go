package laboratory

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cybericebox/laboratory/internal/profiles"
)

// PodSecurity is the hardening of the device pods the operator creates (the VPN and gateway pods have a fixed,
// minimal set, see hardenGroupPod).
type PodSecurity struct {
	// UserNamespaces runs device pods in their own user namespace (hostUsers: false), so root in a device is not
	// root on the node. Needs Kubernetes 1.33+ and a runtime that supports it (containerd 2.x, kernel 6.3+).
	UserNamespaces bool
	// EphemeralStorage is the limit (and request) of a device's writable layer, logs and emptyDirs; empty: none.
	EphemeralStorage string
}

// capsOf is a drop-ALL capability set with exactly these capabilities added, sorted and without duplicates.
func capsOf(add ...string) *corev1.Capabilities {
	seen := map[string]bool{}
	caps := make([]corev1.Capability, 0, len(add))
	for _, c := range add {
		if !seen[c] {
			seen[c] = true
			caps = append(caps, corev1.Capability(c))
		}
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	return &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: caps}
}

// hardenPod applies what every lab pod gets: the runtime's default seccomp profile, no service links (the
// environment variables that list every Service of the namespace), and the service account token only when the pod
// talks to the API server (VPN, gateway) and never for a device, where the participant is root.
func hardenPod(spec *corev1.PodSpec, automountToken bool) {
	spec.AutomountServiceAccountToken = &automountToken
	noLinks := false
	spec.EnableServiceLinks = &noLinks
	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
}

// withEphemeralStorage adds the ephemeral-storage limit (and request) to a container's resources.
func withEphemeralStorage(res corev1.ResourceRequirements, size string) corev1.ResourceRequirements {
	if size == "" {
		return res
	}
	q, err := resource.ParseQuantity(size)
	if err != nil || q.Sign() <= 0 {
		return res
	}
	if res.Limits == nil {
		res.Limits = corev1.ResourceList{}
	}
	if res.Requests == nil {
		res.Requests = corev1.ResourceList{}
	}
	res.Limits[corev1.ResourceEphemeralStorage] = q
	res.Requests[corev1.ResourceEphemeralStorage] = q.DeepCopy()
	return res
}

// hardenGroupContainer is the security context of the VPN and gateway containers: root with exactly the listed
// capabilities, no privilege escalation. (They stay root: they configure interfaces and iptables.)
func hardenGroupContainer(c *corev1.Container, caps ...string) {
	noEscalation := false
	c.SecurityContext = &corev1.SecurityContext{Capabilities: capsOf(caps...), AllowPrivilegeEscalation: &noEscalation}
}

// The capabilities of the group pods: WireGuard and iptables need NET_ADMIN (and NET_RAW for the probe sockets);
// the gateway's DHCP server also binds port 67.
var (
	vpnCaps     = []string{"NET_ADMIN", "NET_RAW"}
	gatewayCaps = []string{"NET_ADMIN", "NET_RAW", "NET_BIND_SERVICE"}
)

// hardenGroupPod brings a VPN or gateway Deployment's pod spec to the hardened shape and says whether it changed:
// the seccomp profile, no service links, the token kept (they use the API), drop ALL with the minimal capabilities,
// and no privileged init container (the conntrack accounting switch is set by the node-agent instead).
func hardenGroupPod(spec *corev1.PodSpec, container string, caps []string) bool {
	before := spec.DeepCopy()
	hardenPod(spec, true)
	keep := spec.InitContainers[:0]
	for _, c := range spec.InitContainers {
		if c.Name != "conntrack-accounting" {
			keep = append(keep, c)
		}
	}
	spec.InitContainers = keep
	if len(spec.InitContainers) == 0 {
		spec.InitContainers = nil
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == container {
			hardenGroupContainer(&spec.Containers[i], caps...)
		}
	}
	return !equality.Semantic.DeepEqual(before, spec)
}

// withTUN adds the extended resource that makes the kubelet pass /dev/net/tun (and only that) to the container: the
// node-agent is its device plugin. A pod that requests it can only run on a node that advertises it.
func withTUN(res corev1.ResourceRequirements, tun bool) corev1.ResourceRequirements {
	if !tun {
		return res
	}
	one := resource.MustParse("1")
	if res.Limits == nil {
		res.Limits = corev1.ResourceList{}
	}
	if res.Requests == nil {
		res.Requests = corev1.ResourceList{}
	}
	res.Limits[corev1.ResourceName(profiles.TUNResource)] = one
	res.Requests[corev1.ResourceName(profiles.TUNResource)] = one.DeepCopy()
	return res
}
