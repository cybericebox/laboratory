package egress

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeCIDRs(t *testing.T) {
	got, err := NormalizeCIDRs([]string{" 10.1.2.3/8 ", "", "203.0.113.0/24"})
	if err != nil || !reflect.DeepEqual(got, []string{"10.0.0.0/8", "203.0.113.0/24"}) {
		t.Fatalf("got %v err %v", got, err)
	}
	for _, bad := range []string{"10.0.0.0", "nope", "fd00::/8", "10.0.0.0/33"} {
		if _, err := NormalizeCIDRs([]string{bad}); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// The default list keeps the metadata service, the private ranges and CGNAT out of reach and leaves the public internet.
func TestDefaultDenyCIDRs(t *testing.T) {
	list, err := NormalizeCIDRs(strings.Split(DefaultDenyCIDRs, ","))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "127.0.0.0/8"} {
		found := false
		for _, c := range list {
			found = found || c == want
		}
		if !found {
			t.Errorf("%s missing from the default deny list", want)
		}
	}
}
