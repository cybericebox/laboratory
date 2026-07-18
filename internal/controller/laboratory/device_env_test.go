package laboratory

import (
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestDeviceEnv(t *testing.T) {
	if got := deviceEnv(&laboratoryv1alpha1.Device{}); got != nil {
		t.Errorf("expected nil env when unset, got %+v", got)
	}

	d := &laboratoryv1alpha1.Device{}
	d.Spec.Env = []laboratoryv1alpha1.EnvVar{
		{Name: "FLAG", Value: "CTF{x}"},
		{Name: "MODE", Value: "hard"},
	}
	env := deviceEnv(d)
	if len(env) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(env))
	}
	if env[0].Name != "FLAG" || env[0].Value != "CTF{x}" {
		t.Errorf("env[0] wrong: %+v", env[0])
	}
	if env[1].Name != "MODE" || env[1].Value != "hard" {
		t.Errorf("env[1] wrong: %+v", env[1])
	}
}
