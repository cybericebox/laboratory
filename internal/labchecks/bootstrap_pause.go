package labchecks

import "time"

// BootstrapPause is how long a device waits after the given number of failed bootstrap attempts before it tries again
// (10 s after the first, 30 s after the second). After BootstrapMaxAttempts failures there is no next attempt.
func BootstrapPause(failedAttempts int) time.Duration {
	switch {
	case failedAttempts <= 0:
		return 0
	case failedAttempts == 1:
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}
