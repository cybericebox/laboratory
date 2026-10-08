package config

import "testing"

func TestLifecycleCapabilityDependencies(t *testing.T) {
	if err := (&Config{}).ValidateLifecycle(); err != nil {
		t.Fatal("default false rejected", err)
	}
	for _, cfg := range []Config{{PerLabStopAvailable: true}, {RuntimeObservation: true, PerLabStopAvailable: true}, {RuntimeObservation: true, ConfirmedRuntimeAvailable: true, RetainedRestartAvailable: true}, {RuntimeObservation: true, ConfirmedRuntimeAvailable: true, FullGroupStopAvailable: true}} {
		if cfg.ValidateLifecycle() == nil {
			t.Fatal("unsupported combination accepted")
		}
	}
	supported := Config{RuntimeObservation: true, StatePersistence: true, PerLabStopAvailable: true, ConfirmedRuntimeAvailable: true, RequiredSnapshotAvailable: true, RequiredSnapshotAdvertised: true, RetainedRestartAvailable: true, FullGroupStopAvailable: true}
	acceptance := Config{RuntimeObservation: true, StatePersistence: true, RequiredSnapshotAvailable: true}
	if err := acceptance.ValidateLifecycle(); err != nil {
		t.Fatal("bounded acceptance before advertisement", err)
	}
	if err := supported.ValidateLifecycle(); err != nil {
		t.Fatal(err)
	}
}
