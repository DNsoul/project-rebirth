package enroll

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"time"

	"awgrotator/internal/proto"
)

// ServerAddr is where a server answers enrollment, as seen from inside the
// tunnel: the server's own tunnel address.
var ServerAddr = netip.AddrPortFrom(netip.MustParseAddr("10.8.0.1"), ListenPort)

// DialFunc opens a connection through the enrollment tunnel. In an app it is
// the tunnel's own network; in a test, an in-memory one.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Refused is returned when the server answered and said no.
type Refused struct{ Reason string }

func (e *Refused) Error() string {
	switch e.Reason {
	case proto.EnrollFull:
		return "the server already holds as many devices as it will"
	case proto.EnrollKeyInUse:
		return "this key already belongs to another device"
	case proto.EnrollBusy:
		return "the server is busy"
	case proto.EnrollPending:
		return "the device is waiting for the operator's approval"
	case proto.EnrollDenied:
		return "the operator has refused this device"
	default:
		return "the server refused: " + e.Reason
	}
}

// Request is what a device asks for.
type Request struct {
	DeviceID  []byte // proto.DeviceIDSize bytes
	PublicKey []byte // proto.TunnelKeySize bytes
	Name      string

	// ContentKey opens and seals the messages.
	ContentKey []byte

	// Server defaults to ServerAddr.
	Server netip.AddrPort

	// Now defaults to time.Now.
	Now func() time.Time
}

func (r Request) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Enroll asks the server for an address and returns it, with prefix.
//
// The datagram is sent again every second until an answer comes: the first
// packets after a handshake can be lost, and a request that is delivered twice
// does no harm. A "busy" answer is waited out with a short random pause and a
// fresh request. Anything else the server says is final.
func Enroll(ctx context.Context, dial DialFunc, r Request) (string, error) {
	server := r.Server
	if !server.IsValid() {
		server = ServerAddr
	}
	conn, err := dial(ctx, "udp", server.String())
	if err != nil {
		return "", fmt.Errorf("reach the server's registration: %w", err)
	}
	defer conn.Close()

	for {
		req, err := proto.NewEnrollRequest(r.DeviceID, r.PublicKey, r.Name, r.now())
		if err != nil {
			return "", err
		}
		blob, err := req.Seal(r.ContentKey)
		if err != nil {
			return "", err
		}
		reply, err := exchange(ctx, conn, []byte(blob), r.ContentKey, req.Nonce)
		if err != nil {
			return "", err
		}
		switch reply.Error {
		case "":
			return reply.Address, nil
		case proto.EnrollBusy:
			pause := time.Second + time.Duration(rand.Int64N(int64(3*time.Second)))
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("the server stayed busy: %w", ctx.Err())
			case <-time.After(pause):
			}
		default:
			return "", &Refused{Reason: reply.Error}
		}
	}
}

// exchange sends the datagram until a matching reply arrives.
func exchange(ctx context.Context, conn net.Conn, datagram []byte, key []byte, nonce string) (*proto.EnrollResponse, error) {
	buf := make([]byte, 2048)
	for attempt := 1; ; attempt++ {
		if _, err := conn.Write(datagram); err != nil {
			return nil, fmt.Errorf("send: %w", err)
		}
		deadline := time.Now().Add(time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = conn.SetReadDeadline(deadline)

		for {
			n, err := conn.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break // send it again
				}
				return nil, fmt.Errorf("read: %w", err)
			}
			reply, err := proto.OpenEnrollResponse(key, string(buf[:n]), nonce)
			if err != nil {
				continue // not ours; keep listening until the deadline
			}
			return reply, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("no answer from the server after %d attempts: %w", attempt, ctx.Err())
		default:
		}
	}
}
