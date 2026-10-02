package snapshot

// IDMapping is one line of a uid_map or gid_map: the Size ids from ContainerID inside the container are HostID.. on the host.
type IDMapping struct {
	ContainerID uint32
	HostID      uint32
	Size        uint32
}

// IDMap is the id mapping of a user namespace. An empty map (no user namespace) changes nothing.
type IDMap []IDMapping

// ToContainer translates a host id to the id inside the container. ok is false when the id is outside the map.
func (m IDMap) ToContainer(host int) (id int, ok bool) {
	if len(m) == 0 {
		return host, true
	}
	if host < 0 {
		return 0, false
	}
	h := uint64(host)
	for _, l := range m {
		if h >= uint64(l.HostID) && h < uint64(l.HostID)+uint64(l.Size) {
			return int(uint64(l.ContainerID) + (h - uint64(l.HostID))), true
		}
	}
	return 0, false
}

// IDMaps are the user and group maps of one container.
type IDMaps struct {
	UID, GID IDMap
}

// translate rewrites the owner of a tar header to container-relative ids; it returns how many ids were outside the map (they become 0).
func (m IDMaps) translate(uid, gid int) (nuid, ngid, unmapped int) {
	nuid, ok := m.UID.ToContainer(uid)
	if !ok {
		nuid, unmapped = 0, unmapped+1
	}
	ngid, ok = m.GID.ToContainer(gid)
	if !ok {
		ngid, unmapped = 0, unmapped+1
	}
	return nuid, ngid, unmapped
}
