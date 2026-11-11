package checker

// IDRange maps ContainerID..ContainerID+Size-1 inside a container to HostID.. on the node (a user namespace, linux.uidMappings of
// the OCI spec).
type IDRange struct {
	ContainerID uint32
	HostID      uint32
	Size        uint32
}

// IDMap translates the numbers the node sees on the files of a container into the numbers the container uses. An empty map is
// the identity (a container without a user namespace).
type IDMap struct {
	UIDs []IDRange
	GIDs []IDRange
}

// UID translates a host user number; ok is false when the container has no such user (the file belongs to somebody unmapped).
func (m IDMap) UID(host uint32) (uint32, bool) { return translate(m.UIDs, host) }

// GID translates a host group number.
func (m IDMap) GID(host uint32) (uint32, bool) { return translate(m.GIDs, host) }

func translate(ranges []IDRange, host uint32) (uint32, bool) {
	if len(ranges) == 0 {
		return host, true
	}
	for _, r := range ranges {
		if host >= r.HostID && uint64(host) < uint64(r.HostID)+uint64(r.Size) {
			return r.ContainerID + (host - r.HostID), true
		}
	}
	return 0, false
}
