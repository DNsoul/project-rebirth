package usage

import (
	"net/netip"
	"sync"
)

// Bytes is traffic both ways: Down to the device, Up from it.
type Bytes struct {
	Down uint64 `json:"d,omitempty"`
	Up   uint64 `json:"u,omitempty"`
}

// Total is both ways together.
func (b Bytes) Total() uint64 { return b.Down + b.Up }

func (b *Bytes) add(o Bytes) {
	b.Down += o.Down
	b.Up += o.Up
}

// Flow is the traffic between one device's tunnel address and one address
// beyond the server.
type Flow struct {
	Client, Remote netip.Addr
}

// Meter counts the packets crossing the tunnel device, by flow. It sits on the
// path of every packet, so it does as little as it can: two addresses and a
// length are read from each, under one lock per batch.
type Meter struct {
	mu    sync.Mutex
	flows map[[8]byte]*Bytes
}

// NewMeter returns an empty meter.
func NewMeter() *Meter { return &Meter{flows: map[[8]byte]*Bytes{}} }

// Count adds the first n packets of a batch, each starting at offset in its
// buffer. Packets with down set are on their way to the devices; the others
// come from them. sizes gives each packet's length; nil means the rest of its
// buffer. Anything that is not IPv4 is not counted: the tunnel carries none.
func (m *Meter) Count(bufs [][]byte, sizes []int, offset, n int, down bool) {
	if n > len(bufs) {
		n = len(bufs)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < n; i++ {
		buf := bufs[i]
		if offset >= len(buf) {
			continue
		}
		pkt := buf[offset:]
		size := len(pkt)
		if sizes != nil {
			size = sizes[i]
		}
		if size < 20 || len(pkt) < 20 || pkt[0]>>4 != 4 {
			continue
		}
		var key [8]byte
		if down {
			copy(key[:4], pkt[16:20]) // to the device
			copy(key[4:], pkt[12:16]) // from the remote end
		} else {
			copy(key[:4], pkt[12:16])
			copy(key[4:], pkt[16:20])
		}
		b := m.flows[key]
		if b == nil {
			b = &Bytes{}
			m.flows[key] = b
		}
		if down {
			b.Down += uint64(size)
		} else {
			b.Up += uint64(size)
		}
	}
}

// Drain returns what has been counted since the last call and starts again.
func (m *Meter) Drain() map[Flow]Bytes {
	m.mu.Lock()
	flows := m.flows
	m.flows = make(map[[8]byte]*Bytes, len(flows))
	m.mu.Unlock()

	out := make(map[Flow]Bytes, len(flows))
	for k, b := range flows {
		f := Flow{Client: netip.AddrFrom4([4]byte(k[:4])), Remote: netip.AddrFrom4([4]byte(k[4:]))}
		out[f] = *b
	}
	return out
}
