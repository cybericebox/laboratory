package netattach

import (
	"strings"
	"testing"
)

func TestValidateInterfaceName(t *testing.T) {
	for _, ok := range []string{"eth0", "eth1", "lab1", "a", "mgmt-0", "abcdefghijklmno"} {
		if err := ValidateInterfaceName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "lo", "accessport", "1eth", "Eth1", "a@x", "a,b", "a|b", "a b", "-a", "abcdefghijklmnop", "eth1\n", "a/b"} {
		if ValidateInterfaceName(bad) == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestValidateMAC(t *testing.T) {
	for _, ok := range []string{"", "random", "02:00:00:00:00:01", "aa:bb:cc:dd:ee:ff", "AA-BB-CC-DD-EE-FE"} {
		if err := ValidateMAC(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"ff:ff:ff:ff:ff:ff", "01:00:5e:00:00:01", "33:33:00:00:00:01", "00:00:00:00:00:00", "xx", "aa:bb:cc:dd:ee", "aa:bb:cc:dd:ee:ff:00:11", "random|x", "aa:bb:cc:dd:ee:ff,eth0@y"} {
		if ValidateMAC(bad) == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestEncodeParseRoundTrip(t *testing.T) {
	in := []Attachment{{Iface: "eth1"}, {Iface: "eth2", MAC: "aa:bb:cc:dd:ee:ff"}, {Iface: "lab1", Name: "port"}}
	out := Parse(Encode(in))
	if len(out) != len(in) {
		t.Fatalf("got %+v", out)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("entry %d: %+v != %+v", i, out[i], in[i])
		}
	}
	if Encode(nil) != "" {
		t.Error("an empty list is the empty annotation")
	}
}

// A hostile name is data in the JSON form: it never becomes a second entry.
func TestEncodeNoSeparatorInjection(t *testing.T) {
	hostile := "a@x,eth0@y|ff:ff:ff:ff:ff:ff"
	got := Parse(Encode([]Attachment{{Iface: hostile, MAC: "aa:bb:cc:dd:ee:ff,eth0@z"}}))
	if len(got) != 1 || got[0].Iface != hostile {
		t.Fatalf("got %+v", got)
	}
	if strings.Contains(Encode(nil), "@") {
		t.Fatal("unreachable")
	}
}

func TestParseLegacy(t *testing.T) {
	got := Parse("eth1@,eth2@|random,lab1@port,x@default")
	want := []Attachment{{Iface: "eth1"}, {Iface: "eth2"}, {Iface: "lab1", Name: "port"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: %+v != %+v", i, got[i], want[i])
		}
	}
	if Parse("[not json") != nil {
		t.Error("a broken JSON value yields nothing")
	}
}

func TestWithWithout(t *testing.T) {
	a := Attachment{Iface: "lab1", Name: "p"}
	l := With(nil, a)
	if len(l) != 1 || len(With(l, a)) != 1 {
		t.Fatal("With is idempotent")
	}
	if len(Without(l, a)) != 0 {
		t.Fatal("Without removes")
	}
}

// L-12: the MAC "random" passes validation but is no hardware address: it reads as no MAC, so CNI ADD does not fail on it.
func TestRandomMACReadsAsNoMAC(t *testing.T) {
	for _, in := range []string{`[{"iface":"eth1","mac":"random"}]`, "eth1@|random"} {
		got := Parse(in)
		if len(got) != 1 || got[0].MAC != "" {
			t.Errorf("%q: %+v", in, got)
		}
	}
	if got := Parse(`[{"iface":"eth1","mac":"02:00:00:00:00:01"}]`); got[0].MAC != "02:00:00:00:00:01" {
		t.Errorf("a real MAC stays: %+v", got)
	}
}
