//go:build !linux

package devicestate

// syncFilesystem is a no-op off Linux (the runtime implementation is Linux-only).
func syncFilesystem(string) error { return nil }
