package grpc

import (
	"strings"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labchecks"
)

// A device that the launcher starts (a service command or args, or a bootstrap) must name its service user: a named user (the launcher
// drops to it), or "root" written out. Without one the launcher, which runs as root, would leave the service root whatever the image
// says. The launcher's own drop and "root stays root" are tested in internal/launcher.
func TestTheLauncherPathNeedsAServiceUser(t *testing.T) {
	spec := func(s *laboratoryv1alpha1.DeviceService) *laboratoryv1alpha1.LabSpec {
		return &laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Service: s}}}
	}
	for name, tc := range map[string]struct {
		service *laboratoryv1alpha1.DeviceService
		refused bool
	}{
		"a user":                  {&laboratoryv1alpha1.DeviceService{Command: []string{"/srv"}, User: "app"}, false},
		"a numeric user":          {&laboratoryv1alpha1.DeviceService{Command: []string{"/srv"}, User: "1000:1000"}, false},
		"root, written out":       {&laboratoryv1alpha1.DeviceService{Command: []string{"/srv"}, User: "root"}, false},
		"root as uid 0":           {&laboratoryv1alpha1.DeviceService{Command: []string{"/srv"}, User: "0"}, false},
		"root as uid:gid 0:0":     {&laboratoryv1alpha1.DeviceService{Command: []string{"/srv"}, User: "0:0"}, false},
		"no user":                 {&laboratoryv1alpha1.DeviceService{Command: []string{"/srv"}}, true},
		"args only, no user":      {&laboratoryv1alpha1.DeviceService{Args: []string{"-x"}}, true},
		"a working dir alone":     {&laboratoryv1alpha1.DeviceService{WorkingDir: "/srv"}, false},
		"no service, no launcher": {nil, false},
	} {
		err := validateDeviceExtras(spec(tc.service))
		if (err != nil) != tc.refused {
			t.Errorf("%s: %v", name, err)
		} else if err != nil && !strings.Contains(err.Error(), "the user of the image, or root named explicitly") {
			t.Errorf("%s: the refusal does not say what to send: %v", name, err)
		}
	}
	// a bootstrap starts the launcher too: it needs a service with a user
	lists := bootstrapLists{"web": &labchecks.Bootstrap{Script: "true"}}
	for name, tc := range map[string]struct {
		service *laboratoryv1alpha1.DeviceService
		refused bool
	}{
		"bootstrap with a user":       {&laboratoryv1alpha1.DeviceService{Command: []string{"/srv"}, User: "root"}, false},
		"bootstrap without a user":    {&laboratoryv1alpha1.DeviceService{Command: []string{"/srv"}}, true},
		"bootstrap without a service": {nil, true},
	} {
		if err := validateBootstrapLists(lists, spec(tc.service)); (err != nil) != tc.refused {
			t.Errorf("%s: %v", name, err)
		}
	}
}
