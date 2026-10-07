package vpn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

type ForwardRule struct {
	ClientName, LabName string
	ClientCIDR, LabCIDR string
	LabInterface        string
	BindingID           string
}

type ForwardPlan struct {
	Allows    []ForwardRule
	Decisions []AccessRule
}

// Reserved only in the VPN pod's conntrack namespace. Other mark bits remain intact.
const FlowCountedMark uint32 = 0x80000000
const LabIdentityMask uint32 = 0x00ffff00

// LabInterfaceIndex rejects wildcard/injected interface names and bounds the
// index used for identifying the actual lab leg in the VPN namespace.
func LabInterfaceIndex(iface string) (uint16, error) {
	if !strings.HasPrefix(iface, "lab") || len(iface) <= 3 || len(iface) > 15 {
		return 0, fmt.Errorf("invalid lab interface %q", iface)
	}
	raw := iface[3:]
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid lab interface %q", iface)
		}
	}
	n, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || strconv.FormatUint(n, 10) != raw {
		return 0, fmt.Errorf("invalid lab interface %q", iface)
	}
	return uint16(n), nil
}

// CompileForwardPlan keeps the complete decision matrix for compatible status,
// but emits only permitted, physically bound relations for the kernel.
func CompileForwardPlan(clients []ClientAccessSnapshot, labs map[string]LabAccessSnapshot, policy []AccessPolicyRule) (ForwardPlan, error) {
	plan := ForwardPlan{Decisions: BuildAccessRules(clients, labs, policy)}
	for _, r := range plan.Decisions {
		if r.Action != AccessAllow {
			continue
		}
		f, err := forwardBinding(r)
		if err != nil {
			return ForwardPlan{}, err
		}
		plan.Allows = append(plan.Allows, f)
	}
	return plan, nil
}

func forwardBinding(r AccessRule) (ForwardRule, error) {
	src, ok := parsePrefix(r.SourceCIDR)
	if !ok || !src.Addr().Is4() || src.Bits() != 32 {
		return ForwardRule{}, fmt.Errorf("client %s has invalid host address %q", r.ClientName, r.SourceCIDR)
	}
	dst, err := netip.ParsePrefix(r.DestinationCIDR)
	if err != nil || !dst.Addr().Is4() {
		return ForwardRule{}, fmt.Errorf("lab %s has invalid subnet %q", r.LabName, r.DestinationCIDR)
	}
	iface := r.LabInterface
	if _, err := LabInterfaceIndex(iface); err != nil {
		return ForwardRule{}, fmt.Errorf("lab %s: %w", r.LabName, err)
	}
	f := ForwardRule{ClientName: r.ClientName, LabName: r.LabName,
		ClientCIDR: src.Masked().String(), LabCIDR: dst.Masked().String(), LabInterface: iface}
	sum := sha256.Sum256([]byte(f.ClientName + "\x00" + f.LabName + "\x00" + f.ClientCIDR + "\x00" + f.LabCIDR + "\x00" + f.LabInterface))
	f.BindingID = hex.EncodeToString(sum[:8])
	return f, nil
}
