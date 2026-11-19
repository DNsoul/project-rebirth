package node

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"awgrotator/internal/usage"
)

// fakeTUN hands out the packets it is given to read and keeps what is
// written to it, the way the kernel device would.
type fakeTUN struct {
	tun.Device // unused methods panic, which a test would notice
	toRead     [][]byte
	written    [][]byte
	err        error
}

func (f *fakeTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	n := 0
	for n < len(bufs) && len(f.toRead) > 0 {
		sizes[n] = copy(bufs[n][offset:], f.toRead[0])
		f.toRead = f.toRead[1:]
		n++
	}
	return n, f.err
}

func (f *fakeTUN) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		f.written = append(f.written, append([]byte(nil), b[offset:]...))
	}
	return len(bufs), f.err
}

func ipv4(src, dst string, size int) []byte {
	p := make([]byte, size)
	p[0] = 0x45
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	for i := 20; i < size; i++ {
		p[i] = byte(i)
	}
	return p
}

func TestTheMeterLetsEveryPacketThroughUnchangedAndCountsIt(t *testing.T) {
	const off = 16
	down := [][]byte{ipv4("198.18.0.7", "10.8.0.5", 1280), ipv4("198.18.0.7", "10.8.0.5", 40)}
	inner := &fakeTUN{toRead: append([][]byte(nil), down...)}
	m := usage.NewMeter()
	dev := meteredTUN{Device: inner, m: m}

	bufs := [][]byte{make([]byte, off+2000), make([]byte, off+2000), make([]byte, off+2000)}
	sizes := make([]int, 3)
	n, err := dev.Read(bufs, sizes, off)
	if err != nil || n != 2 {
		t.Fatalf("read %d, %v", n, err)
	}
	for i := range n {
		if !bytes.Equal(bufs[i][off:off+sizes[i]], down[i]) {
			t.Errorf("packet %d changed on its way to the device", i)
		}
	}

	up := ipv4("10.8.0.5", "198.18.0.7", 300)
	out := [][]byte{append(make([]byte, off), up...)}
	if n, err := dev.Write(out, off); err != nil || n != 1 {
		t.Fatalf("write %d, %v", n, err)
	}
	if len(inner.written) != 1 || !bytes.Equal(inner.written[0], up) {
		t.Error("a packet from the device changed on its way to the system")
	}

	got := m.Drain()[usage.Flow{Client: netip.MustParseAddr("10.8.0.5"), Remote: netip.MustParseAddr("198.18.0.7")}]
	if got != (usage.Bytes{Down: 1320, Up: 300}) {
		t.Errorf("counted %+v", got)
	}

	// The device's own errors come through as they are.
	inner.err = errors.New("closed")
	if _, err := dev.Read(bufs, sizes, off); err == nil {
		t.Error("an error of the device was lost")
	}
	if _, err := dev.Write(out, off); err == nil {
		t.Error("an error of the device was lost")
	}
}
