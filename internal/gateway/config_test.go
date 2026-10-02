//go:build linux

package gateway

import (
	"reflect"
	"testing"

	"github.com/cybericebox/laboratory/internal/egress"
)

// The code default mirrors the shared constant (and the chart).
func TestConfigDefaultMirrorsTheSharedList(t *testing.T) {
	f, _ := reflect.TypeOf(Config{}).FieldByName("EgressDenyCIDRs")
	if got := f.Tag.Get("envDefault"); got != egress.DefaultDenyCIDRs {
		t.Fatalf("envDefault %q != egress.DefaultDenyCIDRs %q", got, egress.DefaultDenyCIDRs)
	}
}
