package nodecap

import (
	"testing"

	"github.com/cybericebox/laboratory/internal/names"
)

func TestProfilePolicyDefaultsAreToday(t *testing.T) {
	var p ProfilePolicy
	if p.Active() || !p.Supported("") || !p.Supported(ProfileContainer) || p.Supported(ProfileMicroVM) || p.Supported("vm") {
		t.Fatalf("zero policy: %+v", p)
	}
	if len(p.Selector("")) != 0 || !p.NodeRuns(nil, ProfileContainer) {
		t.Fatal("a container device selects no label while the profile labels are off")
	}
}

func TestProfilePolicyNodeSelection(t *testing.T) {
	p := ProfilePolicy{Labels: true, MicroVM: true}
	c := map[string]string{names.LabelProfileContainer: "true"}
	kvm := map[string]string{names.LabelProfileContainer: "true", names.LabelProfileMicroVM: "true", names.LabelRuntimeKataCLH: "true"}
	noImpl := map[string]string{names.LabelProfileMicroVM: "true"}
	for _, tc := range []struct {
		labels  map[string]string
		profile string
		want    bool
	}{
		{c, ProfileContainer, true}, {c, ProfileMicroVM, false}, {kvm, ProfileMicroVM, true}, {kvm, ProfileContainer, true},
		{noImpl, ProfileMicroVM, false}, {noImpl, ProfileContainer, false}, {nil, ProfileContainer, false},
	} {
		if got := p.NodeRuns(tc.labels, tc.profile); got != tc.want {
			t.Errorf("NodeRuns(%v, %s) = %v", tc.labels, tc.profile, got)
		}
	}
	if got := p.Selector(ProfileMicroVM); got[names.LabelProfileMicroVM] != "true" || len(got) != 1 {
		t.Errorf("microvm selector: %v", got)
	}
}

func TestProfileConfigPolicy(t *testing.T) {
	p, err := ProfileConfig{MicroVMEnabled: true, MicroVMOverheadCPU: "50m", MicroVMOverheadMemory: "120Mi", ScaleUpProfiles: []string{"microvm"}}.Policy()
	if err != nil || p.Overhead.CPU != 50 || p.Overhead.Memory != 120<<20 || !p.CanScaleUp(ProfileMicroVM) || p.CanScaleUp(ProfileContainer) || !p.ScaleUpKnown() {
		t.Fatalf("%+v %v", p, err)
	}
	if p, _ = (ProfileConfig{}).Policy(); p.ScaleUpKnown() {
		t.Fatal("no node group list: not known")
	}
	if _, err := (ProfileConfig{ScaleUpProfiles: []string{"vm"}}).Policy(); err == nil {
		t.Fatal("an unknown scale-up profile must fail")
	}
	if _, err := (ProfileConfig{MicroVMEnabled: true, MicroVMOverheadCPU: "x", MicroVMOverheadMemory: "1"}).Policy(); err == nil {
		t.Fatal("a bad overhead must fail")
	}
}
