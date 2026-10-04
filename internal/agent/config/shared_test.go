package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/agent/config"
	"github.com/cybericebox/laboratory/internal/operator"
)

// envDefaults collects every env name of a config struct with its envDefault (recursing into nested and embedded structs).
func envDefaults(t reflect.Type, into map[string]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Type.Kind() == reflect.Struct && f.Tag.Get("env") == "" {
			envDefaults(f.Type, into)
			continue
		}
		if name, _, _ := strings.Cut(f.Tag.Get("env"), ","); name != "" {
			into[name] = f.Tag.Get("envDefault")
		}
	}
}

// One chart value, one name: a variable that the operator and the agent both read (the chart renders it once, for both) has the same
// default in both, so a deploy that leaves it out gets the same number in both.
func TestSharedEnvNamesHaveOneDefault(t *testing.T) {
	op, ag := map[string]string{}, map[string]string{}
	envDefaults(reflect.TypeOf(operator.Config{}), op)
	envDefaults(reflect.TypeOf(config.Config{}), ag)
	shared := 0
	for name, agDefault := range ag {
		opDefault, ok := op[name]
		if !ok {
			continue
		}
		shared++
		if opDefault != agDefault {
			t.Errorf("%s: the operator defaults to %q, the agent to %q", name, opDefault, agDefault)
		}
	}
	if shared < 15 {
		t.Errorf("only %d shared names found: the scan is broken or the names drifted apart again", shared)
	}
}
