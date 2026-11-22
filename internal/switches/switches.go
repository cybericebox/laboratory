// Package switches holds the feature switches of the laboratory (the chart's vpn.enabled, inetGateway.enabled, proxy.enabled with its
// rdp, vnc and terminal accesses, checks.enabled, consoleCables.enabled) and the one rule that says what a Lab may ask for under them. The management agent
// refuses early with a stable reason, the operator refuses what slipped through, and the chart renders the same rule into admission
// policies for the API server: all three say the same thing (see DEPLOY.md, "Feature switches").
package switches

import (
	"fmt"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// Stable reasons of a refusal (the agent puts them in ItemResult.reason; pkg/agent/client has the same constants).
const (
	// ReasonVPNUnavailable: the laboratory runs no VPN (vpn.enabled is off).
	ReasonVPNUnavailable = "VPN_UNAVAILABLE"
	// ReasonInternetUnavailable: the laboratory has no internet gateway (inetGateway.enabled is off).
	ReasonInternetUnavailable = "INTERNET_UNAVAILABLE"
	// ReasonAccessUnavailable: the access (http, terminal, rdp, vnc) is not served: the L7 proxy is off, or that access is.
	ReasonAccessUnavailable = "ACCESS_UNAVAILABLE"
	// ReasonChecksUnavailable: the laboratory has no lab checker (checks.enabled is off).
	ReasonChecksUnavailable = "CHECKS_UNAVAILABLE"
	// ReasonConsoleCablesUnavailable: the laboratory does not serve console lines (consoleCables.enabled is off).
	ReasonConsoleCablesUnavailable = "CONSOLE_CABLES_UNAVAILABLE"
)

// Cluster is what the laboratory serves, from the chart.
type Cluster struct {
	// VPN: the WireGuard demultiplexer and the VPN pods exist. Internet: the internet gateway pods exist.
	VPN, Internet bool
	// L7 is the L7 proxy (proxy.enabled and proxy.l7.enabled). Terminal, RDP and VNC are the accesses that need it besides HTTP, which it
	// serves whenever it runs.
	L7, Terminal, RDP, VNC bool
	// Checks: the lab checker exists.
	Checks bool
	// ConsoleCables: devices may declare console and serial ports and a lab may have console cables (consoleCables.enabled).
	ConsoleCables bool
}

// Refusal is why a Lab cannot be admitted: a stable reason and a text for the person.
type Refusal struct {
	Reason string
	Text   string
}

func (r *Refusal) Error() string { return r.Text }

// Refuse says why the lab spec asks for something the laboratory does not serve; nil when it is fine. The first thing found is returned.
func (c Cluster) Refuse(spec *laboratoryv1alpha1.LabSpec) *Refusal {
	if spec.HasVPN() && !c.VPN {
		return &Refusal{ReasonVPNUnavailable, "the lab has a VPN gateway but this laboratory runs no VPN (vpn.enabled is off)"}
	}
	if spec.HasInternet() && !c.Internet {
		return &Refusal{ReasonInternetUnavailable, "the lab has an Internet gateway but this laboratory has no internet gateway (inetGateway.enabled is off)"}
	}
	if spec.Checks != nil && !c.Checks {
		return &Refusal{ReasonChecksUnavailable, "the lab has checks but this laboratory runs no lab checker (checks.enabled is off)"}
	}
	if spec.UsesConsoleCables() && !c.ConsoleCables {
		return &Refusal{ReasonConsoleCablesUnavailable, "the lab has console or serial ports or console cables but this laboratory does not serve console lines (consoleCables.enabled is off)"}
	}
	for i := range spec.Devices {
		for _, a := range spec.Devices[i].Access {
			if text := c.accessRefusal(a.Kind); text != "" {
				return &Refusal{ReasonAccessUnavailable, fmt.Sprintf("device %q: %s", spec.Devices[i].Name, text)}
			}
		}
	}
	return nil
}

// accessRefusal is why an access kind is not served, "" when it is.
func (c Cluster) accessRefusal(kind laboratoryv1alpha1.AccessKind) string {
	if !c.L7 {
		return fmt.Sprintf("the %s access goes through the L7 proxy, and this laboratory runs none (proxy.enabled is off)", kind)
	}
	switch kind {
	case laboratoryv1alpha1.AccessTerminal:
		if !c.Terminal {
			return "the terminal access is not served by this laboratory (terminal.enabled is off)"
		}
	case laboratoryv1alpha1.AccessRDP:
		if !c.RDP {
			return "the rdp access is not served by this laboratory (proxy.rdp.enabled is off)"
		}
	case laboratoryv1alpha1.AccessVNC:
		if !c.VNC {
			return "the vnc access is not served by this laboratory (proxy.vnc.enabled is off)"
		}
	}
	return ""
}

// Everything is a laboratory with every switch on (what tests and an older chart mean).
func Everything() Cluster {
	return Cluster{VPN: true, Internet: true, L7: true, Terminal: true, RDP: true, VNC: true, Checks: true, ConsoleCables: true}
}
