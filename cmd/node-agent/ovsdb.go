//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// portKey computes a stable OVS port name (max 15 chars) for a device interface.
func portKey(namespace, connection, iface string) string {
	h := sha256.Sum256([]byte(namespace + "/" + connection + "/" + iface))
	return fmt.Sprintf("p%x", h[:4])
}

// genevePortName computes a stable OVS Geneve port name for a remote node address.
func genevePortName(remoteAddr string) string {
	h := sha256.Sum256([]byte(remoteAddr))
	return fmt.Sprintf("gv%x", h[:4])
}

// OVSManager programs the single br-ovs bridge via libovsdb (OVSDB JSON-RPC over Unix socket).
type OVSManager struct {
	bridge string
	client client.Client
	ctx    context.Context
}

func newOVSManager(bridge, sockPath string) (*OVSManager, error) {
	dbModel, err := model.NewClientDBModel("Open_vSwitch", map[string]model.Model{
		"Open_vSwitch": &OVSOpen_vSwitch{},
		"Bridge":       &OVSBridge{},
		"Port":         &OVSPort{},
		"Interface":    &OVSInterface{},
	})
	if err != nil {
		return nil, fmt.Errorf("build OVSDB model: %w", err)
	}

	ovs, err := client.NewOVSDBClient(dbModel,
		client.WithEndpoint("unix:"+sockPath),
		client.WithLeaderOnly(false),
	)
	if err != nil {
		return nil, fmt.Errorf("create OVSDB client: %w", err)
	}

	ctx := context.Background()
	// Retry connect — ovsdb-server may still be starting.
	var connErr error
	for i := 0; i < 30; i++ {
		if connErr = ovs.Connect(ctx); connErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if connErr != nil {
		return nil, fmt.Errorf("connect to OVSDB %s: %w", sockPath, connErr)
	}

	if _, err := ovs.MonitorAll(ctx); err != nil {
		return nil, fmt.Errorf("OVSDB monitor: %w", err)
	}

	m := &OVSManager{bridge: bridge, client: ovs, ctx: ctx}
	return m, m.ensureBridge()
}

func (m *OVSManager) findBridge() (*OVSBridge, error) {
	bridges := []OVSBridge{}
	if err := m.client.List(m.ctx, &bridges); err != nil {
		return nil, fmt.Errorf("list bridges: %w", err)
	}
	for i := range bridges {
		if bridges[i].Name == m.bridge {
			return &bridges[i], nil
		}
	}
	return nil, nil
}

func (m *OVSManager) ensureBridge() error {
	if br, err := m.findBridge(); err != nil {
		return err
	} else if br != nil {
		return nil
	}

	bridgeNamedUUID := "bridge_new"
	bridge := OVSBridge{UUID: bridgeNamedUUID, Name: m.bridge}
	bridgeOps, err := m.client.Create(&bridge)
	if err != nil {
		return fmt.Errorf("create bridge op: %w", err)
	}

	// Mutate Open_vSwitch root row to add bridge reference.
	roots := []OVSOpen_vSwitch{}
	if err := m.client.List(m.ctx, &roots); err != nil {
		return fmt.Errorf("list Open_vSwitch root: %w", err)
	}
	if len(roots) == 0 {
		return fmt.Errorf("Open_vSwitch root row not found")
	}
	mutOps, err := m.client.Where(&roots[0]).Mutate(&roots[0],
		model.Mutation{
			Field:   &roots[0].Bridges,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{bridgeNamedUUID},
		},
	)
	if err != nil {
		return fmt.Errorf("mutate root bridges: %w", err)
	}

	ops := append(bridgeOps, mutOps...)
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return fmt.Errorf("transact ensureBridge: %w", err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return fmt.Errorf("ensureBridge result: %w", err)
	}
	return nil
}

// AddInternalPort creates an OVS internal port in br-ovs (idempotent).
func (m *OVSManager) AddInternalPort(name string) error {
	return m.addPort(name, "internal", nil)
}

// AddGenevePort creates a Geneve tunnel port (idempotent). key=flow means per-flow tun_id.
func (m *OVSManager) AddGenevePort(name, remoteIP string) error {
	return m.addPort(name, "geneve", map[string]string{
		"remote_ip": remoteIP,
		"key":       "flow",
	})
}

// findPort returns the first Port matching by name, or nil if not found.
func (m *OVSManager) findPort(name string) (*OVSPort, error) {
	ports := []OVSPort{}
	if err := m.client.List(m.ctx, &ports); err != nil {
		return nil, fmt.Errorf("list ports: %w", err)
	}
	for i := range ports {
		if ports[i].Name == name {
			return &ports[i], nil
		}
	}
	return nil, nil
}

func (m *OVSManager) addPort(name, ifaceType string, options map[string]string) error {
	// Idempotency: check if port already exists in cache.
	if p, err := m.findPort(name); err != nil {
		return err
	} else if p != nil {
		return nil
	}

	br, err := m.findBridge()
	if err != nil {
		return err
	}
	if br == nil {
		return fmt.Errorf("bridge %q not found", m.bridge)
	}

	ifaceNamedUUID := "iface_new"
	portNamedUUID := "port_new"

	iface := OVSInterface{UUID: ifaceNamedUUID, Name: name, Type: ifaceType, Options: options}
	ifaceOps, err := m.client.Create(&iface)
	if err != nil {
		return fmt.Errorf("create interface op: %w", err)
	}

	port := OVSPort{UUID: portNamedUUID, Name: name, Interfaces: []string{ifaceNamedUUID}}
	portOps, err := m.client.Create(&port)
	if err != nil {
		return fmt.Errorf("create port op: %w", err)
	}

	mutOps, err := m.client.Where(br).Mutate(br,
		model.Mutation{
			Field:   &br.Ports,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{portNamedUUID},
		},
	)
	if err != nil {
		return fmt.Errorf("mutate bridge ports: %w", err)
	}

	ops := append(ifaceOps, append(portOps, mutOps...)...)
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return fmt.Errorf("transact addPort %q: %w", name, err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return fmt.Errorf("addPort %q result: %w", name, err)
	}
	return nil
}

// DelPort removes a port from br-ovs (idempotent).
func (m *OVSManager) DelPort(name string) error {
	p, err := m.findPort(name)
	if err != nil {
		return err
	}
	if p == nil {
		return nil // already gone
	}

	br, err := m.findBridge()
	if err != nil {
		return err
	}

	var ops []ovsdb.Operation
	if br != nil {
		mutOps, err := m.client.Where(br).Mutate(br,
			model.Mutation{
				Field:   &br.Ports,
				Mutator: ovsdb.MutateOperationDelete,
				Value:   []string{p.UUID},
			},
		)
		if err != nil {
			return fmt.Errorf("mutate bridge ports: %w", err)
		}
		ops = append(ops, mutOps...)
	}

	delOps, err := m.client.Where(p).Delete()
	if err != nil {
		return fmt.Errorf("delete port op: %w", err)
	}
	ops = append(ops, delOps...)

	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return fmt.Errorf("transact delPort %q: %w", name, err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return fmt.Errorf("delPort %q result: %w", name, err)
	}
	return nil
}

// PortExists checks whether a named port exists on br-ovs.
func (m *OVSManager) PortExists(name string) (bool, error) {
	p, err := m.findPort(name)
	if err != nil {
		return false, err
	}
	return p != nil, nil
}
