package devicestate

import "golang.org/x/sys/unix"

// syncFilesystem flushes every dirty page of the filesystem that holds dir.
func syncFilesystem(dir string) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Syncfs(fd)
}
