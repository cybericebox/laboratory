package v1alpha1

// TerminalSettings tells whether a device has a console and how it starts: the command (empty means /bin/sh) and the user (empty means
// the user of the image). It is the one place that reads the console settings of a device; the operator (the pod label), the L7 proxy
// (the console route) and the device access service all go through it, so that moving the settings into the unified `access` list
// of the lab contract changes this function and nothing else.
func (d *Device) TerminalSettings() (enabled bool, command []string, user string) {
	t := d.Spec.Terminal
	if t == nil || !t.Enabled {
		return false, nil, ""
	}
	return true, t.Command, t.User
}
