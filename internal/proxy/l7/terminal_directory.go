package l7

import (
	"context"
	"net"
	"strconv"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ServiceNameLabel is the label that ties an EndpointSlice to its Service.
const ServiceNameLabel = "kubernetes.io/service-name"

// EndpointDirectory finds the device access service on a node from the EndpointSlices of its headless Service: the ready endpoint whose
// nodeName is that node. The proxy reads EndpointSlices of the one namespace of the service (an address and a node name, nothing else of a
// pod), and no pod.
func EndpointDirectory(r client.Reader, namespace, service string, port int) ServiceDirectory {
	return func(node string) (string, bool) {
		if node == "" {
			return "", false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var slices discoveryv1.EndpointSliceList
		if err := r.List(ctx, &slices, client.InNamespace(namespace), client.MatchingLabels{ServiceNameLabel: service}); err != nil {
			return "", false
		}
		for i := range slices.Items {
			s := &slices.Items[i]
			if s.AddressType != discoveryv1.AddressTypeIPv4 && s.AddressType != discoveryv1.AddressTypeIPv6 {
				continue
			}
			for _, e := range s.Endpoints {
				if e.NodeName == nil || *e.NodeName != node || len(e.Addresses) == 0 {
					continue
				}
				// a missing ready condition means ready, as for any consumer of the API
				if e.Conditions.Ready != nil && !*e.Conditions.Ready {
					continue
				}
				if e.Conditions.Terminating != nil && *e.Conditions.Terminating {
					continue
				}
				return "http://" + net.JoinHostPort(e.Addresses[0], strconv.Itoa(port)), true
			}
		}
		return "", false
	}
}
