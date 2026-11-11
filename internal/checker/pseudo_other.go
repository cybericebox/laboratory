//go:build unix && !linux

package checker

func pseudoFS(int) bool { return false }
