package v1alpha1

// TerminalSettings tells whether a device has a console and how it starts: the command (empty means /bin/sh) and the user (empty means
// the user of the image). It reads the `terminal` entry of the device's access list, and is the one place that does: the operator (the
// pod label), the L7 proxy (the console route) and the device access service all go through it.
func (d *Device) TerminalSettings() (enabled bool, command []string, user string) {
	a := AccessOf(d.Spec.Access, AccessTerminal)
	if a == nil {
		return false, nil, ""
	}
	return true, a.Command, a.User
}

// TerminalFiles tells whether the author switched file transfer through the console on for this device (off unless the terminal entry says
// so). The L7 proxy refuses file requests of a console without it.
func (d *Device) TerminalFiles() bool {
	a := AccessOf(d.Spec.Access, AccessTerminal)
	return a != nil && a.Files
}
