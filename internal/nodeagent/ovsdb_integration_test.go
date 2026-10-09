//go:build linux

package nodeagent

import (
	"testing"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"

	"github.com/cybericebox/laboratory/internal/nstest"
)

// ovs-vswitchd updates statistics beside fields that the node-agent actually
// uses. An unmodeled counter must not discard those updates from the cache.
func TestNetnsOVSCacheUpdatesAlongsideStatistics(t *testing.T) {
	nstest.Require(t)
	m := testOVSDB(t)
	const key = "p0123456789ab"
	if err := m.AddVethPort(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.DelVethPort(key) })
	statistics, err := ovsdb.NewOvsMap(map[string]int{"rx_packets": 1})
	if err != nil {
		t.Fatal(err)
	}
	index, err := ovsdb.NewOvsSet([]int{7})
	if err != nil {
		t.Fatal(err)
	}
	ops := []ovsdb.Operation{{Op: ovsdb.OperationUpdate, Table: "Interface",
		Where: []ovsdb.Condition{ovsdb.NewCondition("name", ovsdb.ConditionEqual, key)},
		Row:   ovsdb.Row{"statistics": statistics, "ifindex": index, "ingress_policing_rate": 100, "ingress_policing_burst": 10},
	}}
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		var ifaces []OVSInterface
		if err := m.client.List(m.ctx, &ifaces); err != nil {
			t.Fatal(err)
		}
		for _, iface := range ifaces {
			if iface.Name == key && iface.Ifindex != nil && *iface.Ifindex == 7 &&
				iface.IngressPolicingRate == 100 && iface.IngressPolicingBurst == 10 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("relevant interface updates were lost when statistics changed: %+v", ifaces)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
