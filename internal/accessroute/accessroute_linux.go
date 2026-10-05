//go:build linux

// Package accessroute keeps the access port of a web-exposed device pod out of the pod's main routing table.
//
// The Kubernetes CNI (Cilium) puts "default via <gw> dev accessport" and "<gw> dev accessport scope link" into the main table.
// A lab interface with a default gateway of its own (the author's, or DHCP's) then replaces that default, and the replies to
// the L7 proxy go into the lab and time out. The fix is source routing, like the sbr CNI meta plugin: every route of the port
// moves into a table of its own, and a rule "from <port address> lookup <table>" sends what leaves with the port's address
// back out of the port. The main table is left with no route through the port, so the pod never uses it for its own traffic.
package accessroute

import (
	"errors"
	"fmt"
	"net"
	"sort"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/cybericebox/laboratory/internal/names"
)

// ErrNoPort means the access port is not in the netns (deleted from inside the pod, or not wired yet).
var ErrNoPort = errors.New("access port not found")

// Result says what Ensure found and did.
type Result struct {
	// Routes is the content of the port's table after the call; keep it and pass it back as known so that a table that was
	// flushed from inside the pod can be restored.
	Routes []netlink.Route
	// Changed is true when something had to be put right: a route moved or restored, a rule added.
	Changed bool
}

// Ensure makes the access port ifname of the netns at netnsPath use the source routing described in the package comment. It is
// idempotent: it moves what is still in the main table (RouteReplace into the port's table, then delete from main), adds the
// rule of every address of the port for IPv4 and IPv6 when absent, and puts back the routes from known (what an earlier call returned) that the table lacks. It fails when the port is not there or has no
// address, or when a route or rule cannot be written.
func Ensure(netnsPath, ifname string, known []netlink.Route) (Result, error) {
	ns, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return Result{}, fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return Result{}, fmt.Errorf("netlink handle in %s: %w", netnsPath, err)
	}
	defer h.Close()
	return ensure(h, ifname, known)
}

func ensure(h *netlink.Handle, ifname string, known []netlink.Route) (Result, error) {
	link, err := h.LinkByName(ifname)
	if err != nil {
		var nf netlink.LinkNotFoundError
		if errors.As(err, &nf) {
			return Result{}, fmt.Errorf("%w: %s", ErrNoPort, ifname)
		}
		return Result{}, fmt.Errorf("find %s: %w", ifname, err)
	}
	var res Result
	addrs := 0
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		changed, n, routes, err := ensureFamily(h, link, family, known)
		if err != nil {
			return res, err
		}
		res.Changed = res.Changed || changed
		res.Routes = append(res.Routes, routes...)
		addrs += n
	}
	if addrs == 0 {
		return res, fmt.Errorf("%s has no address", ifname)
	}
	return res, nil
}

// ensureFamily does Ensure for one address family and says how many global addresses the port has in it. A family without one
// has no rule to add, but its routes (the link-local ones of IPv6) still leave the main table.
func ensureFamily(h *netlink.Handle, link netlink.Link, family int, known []netlink.Route) (changed bool, nAddrs int, _ []netlink.Route, _ error) {
	addrs, err := h.AddrList(link, family)
	if err != nil {
		return false, 0, nil, fmt.Errorf("list addresses of %s: %w", link.Attrs().Name, err)
	}
	var global []netlink.Addr
	for _, a := range addrs {
		if a.IP.IsGlobalUnicast() {
			global = append(global, a)
		}
	}
	idx := link.Attrs().Index
	inMain, err := h.RouteListFiltered(family, &netlink.Route{LinkIndex: idx, Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
	if err != nil {
		return false, 0, nil, fmt.Errorf("list main routes of %s: %w", link.Attrs().Name, err)
	}
	inTable, err := h.RouteListFiltered(family, &netlink.Route{LinkIndex: idx, Table: names.AccessPortRouteTable}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
	if err != nil {
		return false, 0, nil, fmt.Errorf("list table %d routes of %s: %w", names.AccessPortRouteTable, link.Attrs().Name, err)
	}
	if len(global) == 0 && len(inMain) == 0 && len(inTable) == 0 {
		// This family is not in use on the port.
		return false, 0, nil, nil
	}

	want := mergeRoutes(inTable, inMain)
	// What an earlier call saw and the table lacks now (flushed from inside the pod) comes back.
	for _, r := range known {
		if r.LinkIndex == idx && r.Family == family && !hasRoute(want, r) {
			r.Table = names.AccessPortRouteTable
			want = append(want, r)
		}
	}
	// The routes without a gateway first: a route via a gateway needs the route to the gateway to exist.
	sort.SliceStable(want, func(i, j int) bool { return want[i].Gw == nil && want[j].Gw != nil })
	for _, r := range want {
		if hasRoute(inTable, r) {
			continue
		}
		r.Table = names.AccessPortRouteTable
		if err := h.RouteReplace(&r); err != nil {
			return changed, len(global), nil, fmt.Errorf("route %s into table %d: %w", routeString(r), names.AccessPortRouteTable, err)
		}
		changed = true
	}
	for i := range inMain {
		if err := h.RouteDel(&inMain[i]); err != nil && !errors.Is(err, unix.ESRCH) {
			return changed, len(global), nil, fmt.Errorf("remove route %s from the main table: %w", routeString(inMain[i]), err)
		}
		changed = true
	}

	if len(global) == 0 {
		return changed, 0, want, nil
	}
	rules, err := h.RuleList(family)
	if err != nil {
		return changed, len(global), nil, fmt.Errorf("list rules: %w", err)
	}
	for _, a := range global {
		src := hostNet(a.IP)
		if hasRule(rules, src) {
			continue
		}
		rule := netlink.NewRule()
		rule.Family = family
		rule.Src = src
		rule.Table = names.AccessPortRouteTable
		rule.Priority = names.AccessPortRulePriority
		if err := h.RuleAdd(rule); err != nil && !errors.Is(err, unix.EEXIST) {
			return changed, len(global), nil, fmt.Errorf("rule from %s: %w", src, err)
		}
		changed = true
	}
	for i := range want {
		want[i].Table = names.AccessPortRouteTable
	}
	return changed, len(global), want, nil
}

// MainRoutes lists what the main table of the netns still holds through ifname (empty when the port is clean).
func MainRoutes(netnsPath, ifname string) ([]netlink.Route, error) {
	ns, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return nil, fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	link, err := h.LinkByName(ifname)
	if err != nil {
		return nil, fmt.Errorf("find %s: %w", ifname, err)
	}
	var out []netlink.Route
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rs, err := h.RouteListFiltered(family, &netlink.Route{LinkIndex: link.Attrs().Index, Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// mergeRoutes returns the routes of the port's table and those of main that the table lacks.
func mergeRoutes(inTable, inMain []netlink.Route) []netlink.Route {
	out := append([]netlink.Route(nil), inTable...)
	for _, r := range inMain {
		if !hasRoute(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// hasRoute compares routes by destination, gateway and source: the table and the metric are not part of the identity here.
func hasRoute(list []netlink.Route, r netlink.Route) bool {
	for _, x := range list {
		if x.LinkIndex == r.LinkIndex && sameNet(x.Dst, r.Dst) && x.Gw.Equal(r.Gw) {
			return true
		}
	}
	return false
}

func sameNet(a, b *net.IPNet) bool {
	if a == nil || b == nil {
		return a == b || isDefault(a) && isDefault(b)
	}
	return a.IP.Equal(b.IP) && a.Mask.String() == b.Mask.String()
}

// isDefault is true for the destination of a default route: nil, or 0/0.
func isDefault(n *net.IPNet) bool {
	if n == nil {
		return true
	}
	ones, _ := n.Mask.Size()
	return ones == 0 && n.IP.IsUnspecified()
}

func hasRule(rules []netlink.Rule, src *net.IPNet) bool {
	for _, r := range rules {
		if r.Table == names.AccessPortRouteTable && r.Src != nil && r.Src.IP.Equal(src.IP) {
			return true
		}
	}
	return false
}

func hostNet(ip net.IP) *net.IPNet {
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}
}

func routeString(r netlink.Route) string {
	dst := "default"
	if !isDefault(r.Dst) {
		dst = r.Dst.String()
	}
	if r.Gw != nil {
		return dst + " via " + r.Gw.String()
	}
	return dst
}
