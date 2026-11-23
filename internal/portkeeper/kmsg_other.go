//go:build !linux

package portkeeper

import "errors"

// OSKernelLog has no kernel log outside Linux (the keeper runs in Linux devices only; this keeps the package building elsewhere).
type OSKernelLog struct{}

func (OSKernelLog) Guest() (bool, string) { return false, "not Linux" }

func (OSKernelLog) ReadAll() ([]byte, error) { return nil, errors.New("not Linux") }
