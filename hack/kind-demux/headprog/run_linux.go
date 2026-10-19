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

func run(cmd, device string) error {
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return err
	}
	dirs := map[string]ebpf.AttachType{"ingress": ebpf.AttachTCXIngress, "egress": ebpf.AttachTCXEgress}
	if cmd == "remove" {
		var errs []error
		for dir := range dirs {
			path := filepath.Join(pinDir, fmt.Sprintf("%s-%d", dir, iface.Index))
			l, err := link.LoadPinnedLink(path, nil)
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
	for dir, at := range dirs {
		l, err := link.AttachTCX(link.TCXOptions{Program: prog, Interface: iface.Index, Attach: at, Anchor: link.Head()})
		if err != nil {
			return fmt.Errorf("attach %s: %w", dir, err)
		}
		if err := l.Pin(filepath.Join(pinDir, fmt.Sprintf("%s-%d", dir, iface.Index))); err != nil {
			_ = l.Close()
			return fmt.Errorf("pin %s: %w", dir, err)
		}
		_ = l.Close()
	}
	return nil
}
