//go:build linux

package nodeagent

// OVS OVSDB schema model types used by libovsdb.
// Field tags match the OVS 3.3 schema column names exactly.

type OVSOpen_vSwitch struct {
	UUID    string   `ovsdb:"_uuid"`
	Bridges []string `ovsdb:"bridges"`
}

type OVSBridge struct {
	UUID  string   `ovsdb:"_uuid"`
	Name  string   `ovsdb:"name"`
	Ports []string `ovsdb:"ports"`
	// FailMode is "secure" on our bridge: without a flow a frame is dropped, never switched (the standalone
	// default installs a NORMAL flow, which would bridge the ports of different teams).
	FailMode *string `ovsdb:"fail_mode"`
}

type OVSPort struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Interfaces  []string          `ovsdb:"interfaces"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

type OVSInterface struct {
	UUID    string            `ovsdb:"_uuid"`
	Name    string            `ovsdb:"name"`
	Type    string            `ovsdb:"type"`
	Options map[string]string `ovsdb:"options"`
	Ifindex *int              `ovsdb:"ifindex"`
}
