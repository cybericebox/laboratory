package checker

// FileFacts is what the in-device program saw of one file. UID and GID are the numbers inside the device, User and Group their names when
// the device's /etc/passwd and /etc/group give one.
type FileFacts struct {
	Exists  bool
	Regular bool
	// Type is what the path itself is (file, directory, symlink, and so on).
	Type string
	// Mode holds the permission bits including setuid, setgid and sticky.
	Mode      uint32
	UID, GID  uint32
	User      string
	Group     string
	Content   []byte
	Truncated bool
	// Unmapped is set when the owner is not known in the device (the file belongs to nobody the device knows).
	Unmapped bool
}
