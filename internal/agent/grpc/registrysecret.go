package grpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	// maxRegistryCredentials bounds the pull credentials of one variant or prewarm call.
	maxRegistryCredentials = 8
	maxRegistryUsernameLen = 256
	maxRegistryPasswordLen = 4096
)

// registryHostRe is a registry host with an optional port: no scheme, no path.
var registryHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// validateRegistryCredentials checks pull credentials sent by the platform. The errors
// name the entry and its registry host, never a username or password.
func validateRegistryCredentials(creds []*protobuf.RegistryCredential) error {
	if len(creds) > maxRegistryCredentials {
		return invalid("registry_credentials: at most %d entries, got %d", maxRegistryCredentials, len(creds))
	}
	seen := make(map[string]bool, len(creds))
	for i, c := range creds {
		if c == nil {
			return invalid("registry_credentials[%d]: empty entry", i)
		}
		host := c.GetRegistry()
		if host == "" {
			return invalid("registry_credentials[%d]: registry is required", i)
		}
		if !registryHostRe.MatchString(host) {
			return invalid("registry_credentials[%d]: registry must be a host with an optional port, without a scheme or path", i)
		}
		key := strings.ToLower(host)
		if seen[key] {
			return invalid("registry_credentials[%d]: registry %q is sent twice", i, host)
		}
		seen[key] = true
		if c.GetPassword() == "" {
			return invalid("registry_credentials[%d] (%s): password is required", i, host)
		}
		if len(c.GetPassword()) > maxRegistryPasswordLen {
			return invalid("registry_credentials[%d] (%s): password is longer than %d bytes", i, host, maxRegistryPasswordLen)
		}
		if len(c.GetUsername()) > maxRegistryUsernameLen || strings.Contains(c.GetUsername(), ":") {
			return invalid("registry_credentials[%d] (%s): username must be at most %d bytes without ':'", i, host, maxRegistryUsernameLen)
		}
	}
	return nil
}

type dockerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// dockerConfigJSON renders validated credentials as the .dockerconfigjson document a
// kubernetes.io/dockerconfigjson Secret carries.
func dockerConfigJSON(creds []*protobuf.RegistryCredential) ([]byte, error) {
	auths := make(map[string]dockerAuth, len(creds))
	for _, c := range creds {
		auths[strings.ToLower(c.GetRegistry())] = dockerAuth{
			Username: c.GetUsername(),
			Password: c.GetPassword(),
			Auth:     base64.StdEncoding.EncodeToString([]byte(c.GetUsername() + ":" + c.GetPassword())),
		}
	}
	return json.Marshal(struct {
		Auths map[string]dockerAuth `json:"auths"`
	}{auths})
}

// labRegistrySecretName is the Lab.spec.imagePullSecret of a lab created with creds: the
// per-lab Secret when there are credentials, otherwise empty. A value the client put in
// spec_json never survives.
func labRegistrySecretName(labName string, creds []*protobuf.RegistryCredential) string {
	if len(creds) == 0 {
		return ""
	}
	return names.LabRegistrySecret(labName)
}

// labOwner is the controller owner reference of the agent's per-lab Secrets: they are
// garbage-collected with the Lab.
func labOwner(lab *laboratoryv1alpha1.Lab) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion:         laboratoryv1alpha1.SchemeGroupVersion.String(),
		Kind:               kindLab,
		Name:               lab.Name,
		UID:                lab.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &controller,
	}
}

// writeRegistrySecret materializes the lab's pull credentials as the dockerconfigjson
// Secret names.LabRegistrySecret, owner-referenced by the Lab. Like the device variable
// Secrets it is write-only for the agent: delete, then create when there are credentials,
// never read back. Without credentials the Secret is removed. The credentials never reach
// a log, an event or a status; errors carry only the API server's message.
func (h *Handler) writeRegistrySecret(ctx context.Context, lab *laboratoryv1alpha1.Lab, creds []*protobuf.RegistryCredential) error {
	secrets := h.k8s.CoreV1().Secrets(lab.Namespace)
	name := names.LabRegistrySecret(lab.Name)
	if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if len(creds) == 0 {
		return nil
	}
	raw, err := dockerConfigJSON(creds)
	if err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       lab.Namespace,
			OwnerReferences: []metav1.OwnerReference{labOwner(lab)},
			Labels:          map[string]string{names.LabelLab: lab.Name},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: raw},
	}
	_, err = secrets.Create(ctx, s, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// Another agent replica handling a resend of the same lab created it between the delete
	// and the create: replace it once more, so this call's credentials win.
	if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	_, err = secrets.Create(ctx, s, metav1.CreateOptions{})
	return err
}
