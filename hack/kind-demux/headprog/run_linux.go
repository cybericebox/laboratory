//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
)

const pinDir = "/sys/fs/bpf/demux-e2e-head"

type direction struct {
	name   string
	attach ebpf.AttachType
}

var directions = []direction{{"ingress", ebpf.AttachTCXIngress}, {"egress", ebpf.AttachTCXEgress}}

func run(cmd, device string) error {
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return err
	}
	if cmd == "remove" {
		return remove(iface.Index)
	}
	if err := attach(iface.Index); err != nil {
		// Nothing half done stays behind: an ingress program without its egress twin is removed too.
		return errors.Join(err, remove(iface.Index))
	}
	fmt.Printf("e2e_head is first on the tcx ingress and egress of %s\n", device)
	return nil
}

func pinPath(dir string, ifindex int) string {
	return filepath.Join(pinDir, fmt.Sprintf("%s-%d", dir, ifindex))
}

// attach puts the program at the head of both directions and checks right away, before anyone else can act, that
// it is first.
func attach(ifindex int) error {
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:         "e2e_head",
		Type:         ebpf.SchedCLS,
		License:      "GPL",
		Instructions: asm.Instructions{asm.Mov.Imm(asm.R0, -1), asm.Return()}, // TCX_NEXT
	})
	if err != nil {
		return err
	}
	defer func() { _ = prog.Close() }()
	if err := os.MkdirAll(pinDir, 0o700); err != nil {
		return err
	}
	for _, d := range directions {
		l, err := link.AttachTCX(link.TCXOptions{Program: prog, Interface: ifindex, Attach: d.attach, Anchor: link.Head()})
		if err != nil {
			return fmt.Errorf("attach %s: %w", d.name, err)
		}
		err = first(l, ifindex, d)
		if err == nil {
			err = l.Pin(pinPath(d.name, ifindex))
		}
		if err != nil {
			_ = l.Close()
			return fmt.Errorf("%s: %w", d.name, err)
		}
		_ = l.Close()
	}
	return nil
}

func first(l link.Link, ifindex int, d direction) error {
	info, err := l.Info()
	if err != nil {
		return err
	}
	r, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: d.attach})
	if err != nil {
		return err
	}
	if len(r.Programs) == 0 {
		return errors.New("no program attached")
	}
	if id, ok := r.Programs[0].LinkID(); !ok || id != info.ID {
		return fmt.Errorf("not first right after the attach (first link %d, ours %d)", id, info.ID)
	}
	return nil
}

func remove(ifindex int) error {
	var errs []error
	for _, d := range directions {
		l, err := link.LoadPinnedLink(pinPath(d.name, ifindex), nil)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, l.Unpin(), l.Detach(), l.Close())
	}
	return errors.Join(errs...)
}
