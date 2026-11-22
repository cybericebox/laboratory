//go:build linux

package portkeeper

import "syscall"

// sockCloexec keeps the test's socket pairs out of the programs the real-PTY tests start.
const sockCloexec = syscall.SOCK_CLOEXEC
