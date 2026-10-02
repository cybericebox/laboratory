// Package devices names and finds the Device objects of a lab.
//
// A Device used to be called "<lab>-<device>", which two different pairs can spell the same way (lab "a-b" with device "c", lab
// "a" with device "b-c"): one lab then adopted, or deleted the env Secret of, the other's device. New devices are named
// "<lab>-<device>-<hash>" where the hash is over the pair with a separator no name can contain, so the name tells the pair apart.
// A Device that exists under the old name keeps it (renaming would recreate its pod), but it is accepted for a pair only when
// its spec says it belongs to that pair.
package devices

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

const stemLen = 20

func clip(s string) string {
	if len(s) > stemLen {
		return s[:stemLen]
	}
	return s
}

// Name is the object name of the device of a lab: readable stems of both names and a hash of the exact pair. Lab and device
// names are DNS labels, which never contain "/", so two pairs never hash the same input.
func Name(lab, device string) string {
	sum := sha256.Sum256([]byte(lab + "/" + device))
	return clip(lab) + "-" + clip(device) + "-" + hex.EncodeToString(sum[:5])
}

// LegacyName is the name devices had before the hash.
func LegacyName(lab, device string) string { return lab + "-" + device }

// Candidates are the object names a device of the pair may have, the current one first.
func Candidates(lab, device string) []string {
	return []string{Name(lab, device), LegacyName(lab, device)}
}

// Belongs says whether a Device is the device of the pair, by what its spec says and not by its name.
func Belongs(d *laboratoryv1alpha1.Device, lab, device string) bool {
	return d.Spec.LabRef == lab && d.Spec.Name == device
}

// Get finds the Device of a lab: under the current name, else under the old one when that object is this pair's. A
// NotFound error means the device does not exist.
func Get(ctx context.Context, r client.Reader, namespace, lab, device string) (*laboratoryv1alpha1.Device, error) {
	for _, n := range Candidates(lab, device) {
		var d laboratoryv1alpha1.Device
		err := r.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, &d)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if Belongs(&d, lab, device) {
			return &d, nil
		}
	}
	return nil, apierrors.NewNotFound(laboratoryv1alpha1.Resource("devices"), Name(lab, device))
}
