package crdcheck

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestVerifyRefusesAnOldCRD(t *testing.T) {
	old := func(context.Context) ([]byte, error) {
		return []byte(`{"properties":{"status":{"properties":{"enrollment":{}}}}}`), nil
	}
	err := Verify(context.Background(), old)
	if err == nil || !strings.Contains(err.Error(), "certificateEpoch") || !strings.Contains(err.Error(), "kubectl apply") {
		t.Fatalf("an old CRD must be refused with the fix: %v", err)
	}
	cur := func(context.Context) ([]byte, error) {
		return []byte(`{"status":{"properties":{"certificateEpoch":{"type":"integer"}}},"spec":{"properties":{"consolePorts":{}}}}`), nil
	}
	// the CRDs of the console cables: without them an operator with the ConsoleCable watch could not run (helm does not apply crds/)
	noConsole := func(context.Context) ([]byte, error) {
		return []byte(`{"status":{"properties":{"certificateEpoch":{"type":"integer"}}}}`), nil
	}
	if err := Verify(context.Background(), noConsole); err == nil || !strings.Contains(err.Error(), "consolePorts") {
		t.Fatalf("CRDs without the console ports must be refused: %v", err)
	}
	if err := Verify(context.Background(), cur); err != nil {
		t.Fatalf("a current CRD: %v", err)
	}
	if err := Verify(context.Background(), func(context.Context) ([]byte, error) { return nil, errors.New("down") }); err == nil {
		t.Fatal("an unreadable schema is not a pass")
	}
}
