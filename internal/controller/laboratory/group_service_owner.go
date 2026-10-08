package laboratory

import (
	"fmt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func checkServiceGroupUID(d *appsv1.Deployment, uid string) error {
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "GROUP_UID" && e.Value != "" && e.Value != uid {
				return fmt.Errorf("service deployment is owned by another group UID")
			}
		}
	}
	return nil
}
func bindServiceGroupUID(d *appsv1.Deployment, uid string) bool {
	if uid == "" {
		return false
	}
	changed := false
	for n := range d.Spec.Template.Spec.Containers {
		if upsertEnv(&d.Spec.Template.Spec.Containers[n], corev1.EnvVar{Name: "GROUP_UID", Value: uid}) {
			changed = true
		}
	}
	return changed
}
