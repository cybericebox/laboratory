//go:build linux

package vpn

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

func nftSetJSON(name string, elems ...string) string {
	return fmt.Sprintf(`{"set":{"family":"ip","name":%q,"table":"cicevpn","elem":[%s]}}`, name, strings.Join(elems, ","))
}

func testElem(lab, client, prefix, comment string, packets, bytes uint64) string {
	return fmt.Sprintf(`{"elem":{"val":{"concat":[%q,%q,{"prefix":{"addr":%q,"len":24}}]},"comment":%q,"counter":{"packets":%d,"bytes":%d}}}`, lab, client, prefix, comment, packets, bytes)
}

// The accounting sets of nft are read into the same rows the iptables relation chains gave: totals per direction from the T sets, the
// attempts from the N sets, the epoch from the element comment.
func TestParseNFTCountersFoldsTheFourSets(t *testing.T) {
	id, epoch := "0123456789abcdef", "fedcba9876543210"
	comment := "cibacct:" + id + ":" + epoch
	doc := `{"nftables":[{"metainfo":{"version":"1.1.3"}},` + strings.Join([]string{
		nftSetJSON("acct_fwd", testElem("lab1", "10.8.0.5", "10.9.3.0", comment, 10, 1000)),
		nftSetJSON("acct_rev", testElem("lab1", "10.8.0.5", "10.9.3.0", comment, 7, 700)),
		nftSetJSON("new_fwd", testElem("lab1", "10.8.0.5", "10.9.3.0", comment, 2, 120)),
		nftSetJSON("new_rev", testElem("lab1", "10.8.0.5", "10.9.3.0", comment, 1, 60)),
		nftSetJSON("allow_fwd"),
	}, ",") + `]}`
	bindings := map[string]ForwardRule{id: {ClientName: "p1", LabName: "a", BindingID: id, ClientCIDR: "10.8.0.5/32"}}
	st, err := parseNFTCounters([]byte(doc), bindings)
	if err != nil || len(st.snapshot.Rows) != 1 || st.snapshot.Partial {
		t.Fatalf("%+v %v", st, err)
	}
	r := st.snapshot.Rows[0]
	if r.PacketsOut != 10 || r.BytesOut != 1000 || r.PacketsIn != 7 || r.BytesIn != 700 || r.Attempts != 2 || r.LabInitiatedAttempts != 1 || r.Epoch != epoch || r.Subject != "p1" || r.Lab != "a" {
		t.Fatalf("row: %+v", r)
	}
	if got := st.keys[id]["acct_fwd"]; len(got) != 1 || got[0] != `"lab1" . 10.8.0.5 . 10.9.3.0/24` {
		t.Fatalf("element keys to delete: %v", st.keys[id])
	}
	// a binding without its four elements is an error, or partial when tolerated
	if _, err := parseNFTCounters([]byte(`{"nftables":[]}`), bindings); err == nil {
		t.Fatal("a missing binding is an error")
	}
	if st, err := parseNFTCounters([]byte(`{"nftables":[]}`), bindings, true); err != nil || !st.snapshot.Partial {
		t.Fatalf("tolerated: %+v %v", st, err)
	}
	// a table that does not exist yet
	if st, err := parseNFTCounters(nil, nil); err != nil || len(st.snapshot.Rows) != 0 {
		t.Fatalf("no table: %+v %v", st, err)
	}
	// a foreign comment is refused
	bad := `{"nftables":[` + nftSetJSON("acct_fwd", testElem("lab1", "10.8.0.5", "10.9.3.0", "something", 1, 1)) + `]}`
	if _, err := parseNFTCounters([]byte(bad), nil); err == nil {
		t.Fatal("an element that is not ours must not be read as ours")
	}
}

type fakeNFT struct {
	scripts []string
	// lists are the answers of List in order; the last one repeats
	lists []string
}

func (f *fakeNFT) Apply(_ context.Context, script []byte) error {
	f.scripts = append(f.scripts, string(script))
	return nil
}
func (f *fakeNFT) List(context.Context) ([]byte, error) {
	answer := f.lists[0]
	if len(f.lists) > 1 {
		f.lists = f.lists[1:]
	}
	return []byte(answer), nil
}

// A plan is one transaction that empties the permission sets and fills them again, and creates the accounting elements of a new pair
// with the identity comment; nothing is conntrack-based.
func TestNFTPlanIsOneTransactionWithoutConntrack(t *testing.T) {
	f := &fakeNFT{}
	m := &IPTablesManager{wgIface: "wg0", nft: f}
	rule, err := forwardBinding(AccessRule{ClientName: "p1", LabName: "a", SourceCIDR: "10.8.0.5/32", DestinationCIDR: "10.9.3.0/24", LabInterface: "lab1", Action: AccessAllow})
	if err != nil {
		t.Fatal(err)
	}
	// the list after the first apply is not the fake's concern: ReadPairCounters is exercised by the netns test; here the retirement read
	// must find the four elements the apply created
	full := `{"nftables":[` + strings.Join([]string{
		nftSetJSON("acct_fwd", testElem("lab1", "10.8.0.5", "10.9.3.0", "cibacct:"+rule.BindingID+":0011223344556677", 0, 0)),
		nftSetJSON("acct_rev", testElem("lab1", "10.9.3.0", "10.8.0.5", "cibacct:"+rule.BindingID+":0011223344556677", 0, 0)),
		nftSetJSON("new_fwd", testElem("lab1", "10.8.0.5", "10.9.3.0", "cibacct:"+rule.BindingID+":0011223344556677", 0, 0)),
		nftSetJSON("new_rev", testElem("lab1", "10.9.3.0", "10.8.0.5", "cibacct:"+rule.BindingID+":0011223344556677", 0, 0)),
	}, ",") + `]}`
	// before the first plan the table is empty; afterwards it holds the pair
	f.lists = []string{`{"nftables":[]}`, full}
	if _, err := m.ApplyForwardPlan(context.Background(), ForwardPlan{Allows: []ForwardRule{rule}}); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 1 {
		t.Fatalf("one transaction expected, got %d", len(f.scripts))
	}
	s := f.scripts[0]
	for _, want := range []string{"flush set ip cicevpn allow_fwd", "add element ip cicevpn allow_fwd { \"lab1\" . 10.8.0.5 . 10.9.3.0/24 }", "add element ip cicevpn allow_rev { \"lab1\" . 10.9.3.0/24 . 10.8.0.5 }", "labnets { 10.9.3.0/24 }", "clients { 10.8.0.5 }"} {
		if !strings.Contains(s, want) {
			t.Errorf("the transaction lacks %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "counter comment \"cibacct:"+rule.BindingID+":") != 4 {
		t.Errorf("a new pair gets its four accounting elements:\n%s", s)
	}
	if strings.Contains(s, "ct state") || strings.Contains(s, "conntrack") {
		t.Errorf("no connection tracking in the gate:\n%s", s)
	}
	// the base script marks the pair traffic notrack and closes the gate by policy
	base := m.nftBaseScript()
	for _, want := range []string{"notrack", "policy drop", "iifname \"wg0\" oifname \"wg0\" drop", "jump count_fwd", "jump count_rev"} {
		if !strings.Contains(base, want) {
			t.Errorf("the base ruleset lacks %q", want)
		}
	}
	// retiring the pair reads its counters and deletes its four elements
	f.scripts = nil
	retired := false
	m.BeforeRetire = func(s flowacct.CounterSnapshot) error { retired = len(s.Rows) == 1; return nil }
	if _, err := m.ApplyForwardPlan(context.Background(), ForwardPlan{}); err != nil {
		t.Fatal(err)
	}
	if !retired {
		t.Error("the retired pair's counters must be handed over before its elements are deleted")
	}
	last := f.scripts[len(f.scripts)-1]
	if strings.Count(last, "delete element ip cicevpn") != 4 {
		t.Errorf("the four elements of the retired pair are deleted:\n%s", last)
	}
}
