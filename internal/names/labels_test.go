package names

import "testing"

func TestUserLabelsDropsThePlatformKeys(t *testing.T) {
	got := UserLabels(map[string]string{"event": "e1", LabelDeployGroup: "g", LabelLab: "l"})
	if len(got) != 1 || got["event"] != "e1" {
		t.Fatalf("got %v", got)
	}
	if UserLabels(nil) == nil {
		t.Fatal("callers add keys to the result")
	}
}
