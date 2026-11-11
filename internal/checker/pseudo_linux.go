//go:build linux

package checker

import "golang.org/x/sys/unix"

// pseudoFS reports a descriptor on a file system that is not a plain store of the container's files: reading it from the node would
// show the node's own view (procfs, sysfs and cgroupfs answer per reader), not the container's.
func pseudoFS(fd int) bool {
	var sfs unix.Statfs_t
	if err := unix.Fstatfs(fd, &sfs); err != nil {
		return true
	}
	switch sfs.Type {
	case unix.PROC_SUPER_MAGIC, unix.SYSFS_MAGIC, unix.CGROUP_SUPER_MAGIC, unix.CGROUP2_SUPER_MAGIC, unix.DEVPTS_SUPER_MAGIC,
		unix.BPF_FS_MAGIC, unix.DEBUGFS_MAGIC, unix.TRACEFS_MAGIC, unix.SECURITYFS_MAGIC, unix.NSFS_MAGIC, unix.SELINUX_MAGIC:
		return true
	}
	return false
}
