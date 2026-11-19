package node

import (
	"strings"
	"testing"
)

func TestTheServersCardsAreReadWithoutLoopbackAndTunnels(t *testing.T) {
	const netdev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:   14555     120    0    0    0     0          0         0    14555     120    0    0    0     0       0          0
  eth0: 459225897328 1 0 0 0 0 0 0 435877355106 2 0 0 0 0 0 0
  ens4:   16216      90    0    0    0     0          0         0    13620      80    0    0    0     0       0          0
  awg0: 8952447880 3 0 0 0 0 0 0 158443539724 4 0 0 0 0 0 0
 warp0: 5 1 0 0 0 0 0 0 6 1 0 0 0 0 0 0
`
	got, err := parseNetDev(strings.NewReader(netdev), []string{"lo", "awg0", "warp", "awg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["eth0"] != [2]uint64{459225897328, 435877355106} || got["ens4"] != [2]uint64{16216, 13620} {
		t.Errorf("cards = %v", got)
	}
	if since(100, 150) != 50 || since(150, 20) != 20 {
		t.Error("since")
	}
}
