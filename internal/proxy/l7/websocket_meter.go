package l7

import (
	"bufio"
	"encoding/binary"
	"net"
	"strings"
	"sync"
)

// frameMeter retains only the at-most-14-byte frame header, never payload.
// Compressed data is measured as relayed; control frames and masks are excluded.
type frameMeter struct {
	mu                    sync.Mutex
	count                 func(int64)
	incomplete            func()
	masked, compression   bool
	header                [14]byte
	have, need            int
	remaining             uint64
	data, fragmented, bad bool
}

func newFrameMeter(count func(int64), incomplete func(), masked, compression bool) *frameMeter {
	return &frameMeter{count: count, incomplete: incomplete, masked: masked, compression: compression, need: 2}
}
func (f *frameMeter) invalid() {
	if !f.bad {
		f.bad = true
		f.incomplete()
	}
}
func (f *frameMeter) feed(p []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(p) > 0 && !f.bad {
		if f.remaining > 0 {
			n := uint64(len(p))
			if n > f.remaining {
				n = f.remaining
			}
			if f.data {
				f.count(int64(n))
			}
			f.remaining -= n
			p = p[int(n):]
			continue
		}
		n := f.need - f.have
		if n > len(p) {
			n = len(p)
		}
		copy(f.header[f.have:], p[:n])
		f.have += n
		p = p[n:]
		if f.have < f.need {
			continue
		}
		if f.need == 2 {
			a, b := f.header[0], f.header[1]
			opcode := a & 15
			fin := a&0x80 != 0
			if a&0x30 != 0 || (a&0x40 != 0 && (!f.compression || opcode != 1 && opcode != 2)) || (b&0x80 != 0) != f.masked {
				f.invalid()
				continue
			}
			switch opcode {
			case 0:
				if !f.fragmented {
					f.invalid()
					continue
				}
				f.data = true
				if fin {
					f.fragmented = false
				}
			case 1, 2:
				if f.fragmented {
					f.invalid()
					continue
				}
				f.data = true
				f.fragmented = !fin
			case 8, 9, 10:
				if !fin || b&127 > 125 {
					f.invalid()
					continue
				}
				f.data = false
			default:
				f.invalid()
				continue
			}
			ext := 0
			switch b & 127 {
			case 126:
				ext = 2
			case 127:
				ext = 8
			}
			f.need = 2 + ext
			if f.masked {
				f.need += 4
			}
			if f.have < f.need {
				continue
			}
		}
		length := uint64(f.header[1] & 127)
		switch length {
		case 126:
			length = uint64(binary.BigEndian.Uint16(f.header[2:4]))
			if length < 126 {
				f.invalid()
				continue
			}
		case 127:
			length = binary.BigEndian.Uint64(f.header[2:10])
			if length < 65536 || length>>63 != 0 {
				f.invalid()
				continue
			}
		}
		f.remaining = length
		f.have = 0
		f.need = 2
	}
}
func (f *frameMeter) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.have != 0 || f.remaining != 0 || f.fragmented {
		f.invalid()
	}
}

// meteredUpgrade reads through the original buffered reader, preserving frames
// already received with the HTTP request. Its new writer meters successful
// socket writes and skips the 101 response header before reading frame metadata.
type meteredUpgrade struct {
	net.Conn
	reader      *bufio.Reader
	in, out     *frameMeter
	writeMu     sync.Mutex
	readMu      sync.Mutex
	headers     bool
	tail        uint32
	headerBytes int
}

func (c *meteredUpgrade) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	n, err := c.reader.Read(p)
	c.out.feed(p[:n])
	return n, err
}
func (c *meteredUpgrade) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n, err := c.Conn.Write(p)
	actual := p[:n]
	if !c.headers {
		for i, b := range actual {
			c.tail = c.tail<<8 | uint32(b)
			c.headerBytes++
			if c.tail == 0x0d0a0d0a {
				c.headers = true
				actual = actual[i+1:]
				break
			}
			if c.headerBytes > 65536 {
				c.in.mu.Lock()
				c.in.invalid()
				c.in.mu.Unlock()
				c.headers = true
				actual = nil
				break
			}
		}
		if !c.headers {
			return n, err
		}
	}
	c.in.feed(actual)
	return n, err
}
func (c *meteredUpgrade) Close() error {
	err := c.Conn.Close()
	c.readMu.Lock()
	defer c.readMu.Unlock()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.in.finish()
	c.out.finish()
	return err
}
func websocketCompression(header string) (compression, known bool) {
	known = true
	for _, value := range strings.Split(header, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(value), ";")
		if name == "" {
			continue
		}
		if strings.EqualFold(name, "permessage-deflate") {
			compression = true
		} else {
			known = false
		}
	}
	return
}
