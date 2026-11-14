package v1alpha1

// InternetConflict is a device with internet: deny that shares a broadcast domain with an Internet gateway.
type InternetConflict struct {
	Device, Gateway string
}

// InternetConflicts lists the devices of the spec that have internet: deny and share a broadcast domain with an Internet gateway. A
// broadcast domain is the set of connections joined through shared switch and hub endpoints; a device or a gateway is a leaf of the
// domain of each connection it is in. The answer is in the order of the devices of the spec. It is one function for the agent (the
// call is refused) and for the operator (the lab is refused and the denied device is cut off), so they never disagree.
func (s *LabSpec) InternetConflicts() []InternetConflict {
	denied := map[string]bool{}
	for _, d := range s.Devices {
		if d.Internet == InternetDeny {
			denied[d.Name] = true
		}
	}
	if len(denied) == 0 {
		return nil
	}
	switches := map[string]bool{}
	for _, d := range s.Devices {
		if d.Type == DeviceTypeUnmanagedSwitch || d.Type == DeviceTypeHub {
			switches[d.Name] = true
		}
	}
	n := len(s.Connections)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	first := map[string]int{} // switch -> the first connection that touches it
	for ci, c := range s.Connections {
		for _, ep := range c.Endpoints {
			if !switches[ep.Device] {
				continue
			}
			if f, ok := first[ep.Device]; ok {
				if a, b := find(f), find(ci); a != b {
					parent[a] = b
				}
			} else {
				first[ep.Device] = ci
			}
		}
	}
	internet := map[int]string{}   // domain -> its Internet gateway
	inDomain := map[int][]string{} // domain -> its denied devices, in the order met
	for ci, c := range s.Connections {
		root := find(ci)
		for _, ep := range c.Endpoints {
			if g, ok := s.Gateway(ep.Device); ok && g.Kind == GatewayKindInternet {
				internet[root] = g.Name
			}
			if denied[ep.Device] {
				inDomain[root] = append(inDomain[root], ep.Device)
			}
		}
	}
	seen := map[string]bool{}
	var out []InternetConflict
	for _, d := range s.Devices {
		for root, devs := range inDomain {
			gw, ok := internet[root]
			if !ok {
				continue
			}
			for _, name := range devs {
				if name == d.Name && !seen[name] {
					seen[name] = true
					out = append(out, InternetConflict{Device: name, Gateway: gw})
				}
			}
		}
	}
	return out
}
