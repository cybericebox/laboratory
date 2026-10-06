package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/netattach"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// deviceVars is device name -> variable name -> value.
type deviceVars map[string]map[string]string

// mergeEnvLists folds DeviceEnv lists into one deviceVars; later lists override earlier
// ones variable by variable. A listed device stays in the result even without variables
// (the Update call reads that as "remove the Secret").
func mergeEnvLists(lists ...[]*protobuf.DeviceEnv) deviceVars {
	out := deviceVars{}
	for _, list := range lists {
		for _, de := range list {
			if de == nil {
				continue
			}
			vars := out[de.Device]
			if vars == nil {
				vars = map[string]string{}
				out[de.Device] = vars
			}
			for k, v := range de.Vars {
				vars[k] = v
			}
		}
	}
	return out
}

// validateEnv checks variable names and, when devices is given, that every device exists.
func validateEnv(env deviceVars, devices map[string]bool) error {
	for _, dev := range sortedKeys(env) {
		if devices != nil && !devices[dev] {
			return fmt.Errorf("env for unknown device %q", dev)
		}
		for k := range env[dev] {
			if errs := validation.IsEnvVarName(k); len(errs) > 0 {
				return fmt.Errorf("device %q: invalid variable name %q: %s", dev, k, strings.Join(errs, "; "))
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func specDevices(spec *laboratoryv1alpha1.LabSpec) map[string]bool {
	out := make(map[string]bool, len(spec.Devices))
	for i := range spec.Devices {
		out[spec.Devices[i].Name] = true
	}
	return out
}

// forbiddenDeviceKeys are device fields that would carry variables (or flags) in the CR.
// Device variables are secrets only; they travel in env, never in spec_json.
var forbiddenDeviceKeys = []string{"env", "envs", "environment", "envfrom", "flag", "flags"}

// parseLabSpec reads spec_json into a LabSpec. It rejects unknown fields and, with a
// clear message, any env/flags field on devices.
func parseLabSpec(raw []byte, persistence bool) (laboratoryv1alpha1.LabSpec, error) {
	var spec laboratoryv1alpha1.LabSpec
	if len(raw) == 0 {
		return spec, fmt.Errorf("spec_json is empty")
	}
	var generic struct {
		Devices []map[string]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		return spec, fmt.Errorf("spec_json: %w", err)
	}
	for i, dev := range generic.Devices {
		for key := range dev {
			for _, bad := range forbiddenDeviceKeys {
				if strings.EqualFold(key, bad) {
					return spec, fmt.Errorf("spec_json: device %d carries %q: device variables are secrets, send them as env", i, key)
				}
			}
		}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, fmt.Errorf("spec_json: %w", err)
	}
	for i := range spec.Devices {
		if err := validatePersistence(&spec.Devices[i], persistence); err != nil {
			return spec, fmt.Errorf("spec_json: device %q: %w", spec.Devices[i].Name, err)
		}
		if err := validateInterfaces(&spec.Devices[i]); err != nil {
			return spec, fmt.Errorf("spec_json: device %q: %w", spec.Devices[i].Name, err)
		}
	}
	return spec, nil
}

// validateInterfaces checks the interface names and MACs of a device: the same rules the
// CRD enforces, reported before anything is created.
func validateInterfaces(d *laboratoryv1alpha1.DeviceTemplate) error {
	for _, iface := range d.Interfaces {
		if err := netattach.ValidateInterfaceName(iface.Name); err != nil {
			return err
		}
		if err := netattach.ValidateMAC(iface.MAC); err != nil {
			return err
		}
	}
	return nil
}

// validatePersistence checks a device's persistence request: enabled only when the
// cluster allows persistence, a positive debounce. The excluded paths and the quota are
// platform settings, not part of the request.
func validatePersistence(d *laboratoryv1alpha1.DeviceTemplate, allowed bool) error {
	p := d.Persistence
	if p == nil {
		return nil
	}
	if p.Enabled && !allowed {
		return fmt.Errorf("persistence.enabled: the cluster does not allow state persistence")
	}
	if p.Debounce != nil && p.Debounce.Duration <= 0 {
		return fmt.Errorf("persistence.debounce must be positive")
	}
	return nil
}

// writeDeviceSecrets materializes per-device variables into write-only Secrets named
// "<device object name>-env" (the name of the Device, which the pod's envFrom uses), owner-referenced by the Lab so they are garbage-collected with it.
// The device pod loads them via envFrom.
//
// The agent only ever CREATEs/DELETEs these Secrets, never reads them, so a device's
// variables (a task flag, a license) live solely in the Secret: never in the Lab/Device
// CR and never back through the agent. "Overwrite" is therefore delete-then-create.
//
// For each device in devices: the Secret is written when it has variables, otherwise
// removed. Writing the same values again is safe.
func (h *Handler) writeDeviceSecrets(ctx context.Context, lab *laboratoryv1alpha1.Lab, devices []string, env deviceVars) error {
	controller := true
	owner := metav1.OwnerReference{
		APIVersion:         laboratoryv1alpha1.SchemeGroupVersion.String(),
		Kind:               "Lab",
		Name:               lab.Name,
		UID:                lab.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &controller,
	}
	secrets := h.k8s.CoreV1().Secrets(lab.Namespace)
	for _, dev := range devices {
		objName, err := h.deviceObjectName(ctx, lab.Namespace, lab.Name, dev)
		if err != nil {
			return err
		}
		name := objName + "-env"
		if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		vars := env[dev]
		if len(vars) == 0 {
			continue
		}
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       lab.Namespace,
				OwnerReferences: []metav1.OwnerReference{owner},
				Labels: map[string]string{
					names.LabelLab:    lab.Name,
					names.LabelDevice: dev,
				},
			},
			StringData: vars,
		}
		if _, err := secrets.Create(ctx, s, metav1.CreateOptions{}); err != nil {
			return err
		}
	}
	return nil
}
