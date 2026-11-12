package grpc

import (
	"strings"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// File transfer through the console is the author's option on the terminal entry. It is refused where it cannot work (a vm profile) and
// where it has no meaning (any other kind of access).
func TestValidateAccessFilesOption(t *testing.T) {
	dev := func(profile laboratoryv1alpha1.DeviceProfile, access ...laboratoryv1alpha1.DeviceAccess) *laboratoryv1alpha1.DeviceTemplate {
		return &laboratoryv1alpha1.DeviceTemplate{Name: "box", Type: laboratoryv1alpha1.DeviceTypeContainer, Profile: profile, Access: access}
	}
	term := func(files bool) laboratoryv1alpha1.DeviceAccess {
		return laboratoryv1alpha1.DeviceAccess{Kind: laboratoryv1alpha1.AccessTerminal, Files: files}
	}
	for _, profile := range []laboratoryv1alpha1.DeviceProfile{"", laboratoryv1alpha1.DeviceProfileContainer, laboratoryv1alpha1.DeviceProfileMicroVM} {
		for _, files := range []bool{false, true} {
			if err := validateAccess(dev(profile, term(files))); err != nil {
				t.Errorf("profile %q files %v: %v", profile, files, err)
			}
		}
	}
	if err := validateAccess(dev(laboratoryv1alpha1.DeviceProfileVM, term(true))); err == nil || !strings.Contains(err.Error(), "not possible on a vm profile") {
		t.Errorf("files on a vm profile: %v", err)
	}
	if err := validateAccess(dev("", laboratoryv1alpha1.DeviceAccess{Kind: laboratoryv1alpha1.AccessHTTP, Port: 80, Files: true})); err == nil || !strings.Contains(err.Error(), "only a terminal access has files") {
		t.Errorf("files on an http access: %v", err)
	}
}
