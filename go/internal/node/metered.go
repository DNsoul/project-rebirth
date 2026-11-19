package node

import (
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"awgrotator/internal/usage"
)

// meteredTUN is the tunnel device with a meter on it: every packet the
// tunnel hands to the system, and every one it takes from it, is counted
// before it goes on unchanged.
type meteredTUN struct {
	tun.Device
	m *usage.Meter
}

// Read takes packets on their way to the devices.
func (t meteredTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	n, err := t.Device.Read(bufs, sizes, offset)
	if n > 0 {
		t.m.Count(bufs, sizes, offset, n, true)
	}
	return n, err
}

// Write hands on packets that came from the devices. They are counted before
// the device is given them: it may join them into larger ones in place.
func (t meteredTUN) Write(bufs [][]byte, offset int) (int, error) {
	t.m.Count(bufs, nil, offset, len(bufs), false)
	return t.Device.Write(bufs, offset)
}
