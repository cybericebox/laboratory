package v1alpha1

import (
	"os"
	"strings"
	"testing"
)

func TestDeviceTypeEnumHasNoVM(t *testing.T) {
	paths := []string{
		"../../../config/crd/bases/laboratory.cybericebox.com_devices.yaml",
		"../../../config/crd/bases/laboratory.cybericebox.com_labs.yaml",
		"../../../charts/laboratory/crds/laboratory.cybericebox.com_devices.yaml",
		"../../../charts/laboratory/crds/laboratory.cybericebox.com_labs.yaml",
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "- vm") {
			t.Errorf("%s still admits vm", path)
		}
	}
}
