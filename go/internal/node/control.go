package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
	"awgrotator/internal/proto"
)

// The control socket is how the manager on the same machine carries out the
// operator's decisions about devices: the device list belongs to this process,
// which also owns the tunnel the devices go into, so the decisions are made
// here and nowhere else. The socket is a file only root can open, and it is
// reached from this machine only.

// maxControlMessage bounds what is read from either side of the socket. The
// overview of a full server is a few kilobytes.
const maxControlMessage = 1 << 20

// DefaultControlPath is where the socket is.
const DefaultControlPath = "/run/awgnode/control.sock"

// Control operations.
const (
	OpOverview = "overview"
	OpApprove  = "approve"
	OpDeny     = "deny"
	OpRevoke   = "revoke"
	OpAllow    = "allow"
	OpForget   = "forget"
	OpNotified = "notified"
)

// ControlRequest is one question or decision. ID is a device id (base64url).
type ControlRequest struct {
	Op  string   `json:"op"`
	ID  string   `json:"id,omitempty"`
	IDs []string `json:"ids,omitempty"`
}

// ControlResponse is the answer. Error is set when the operation did not
// happen; Reason, when it was the device list that said no, is one of the
// proto.Enroll* reasons, and Missing says the device was not where the
// operation expected it (already decided, perhaps from another chat).
type ControlResponse struct {
	OK       bool             `json:"ok"`
	Error    string           `json:"error,omitempty"`
	Reason   string           `json:"reason,omitempty"`
	Missing  bool             `json:"missing,omitempty"`
	Overview *ControlOverview `json:"overview,omitempty"`
	Name     string           `json:"name,omitempty"` // the device acted on
}

// ControlOverview is the device list as the operator sees it: each entry with
// its matching code, and the approved ones with what the tunnel knows of them.
type ControlOverview struct {
	Approval bool          `json:"approval"`
	Devices  []DeviceView  `json:"devices"`
	Service  int           `json:"service"`
	Pending  []WaitingView `json:"pending"`
	Denied   []DenialView  `json:"denied"`
}

// DeviceView is an approved device.
type DeviceView struct {
	enroll.Device
	Code          string `json:"code"`
	Online        bool   `json:"online"`
	LastHandshake int64  `json:"last_handshake,omitempty"`
	RxBytes       int64  `json:"rx_bytes,omitempty"`
	TxBytes       int64  `json:"tx_bytes,omitempty"`
}

// WaitingView is a device waiting for a decision.
type WaitingView struct {
	enroll.Waiting
	Code string `json:"code"`
}

// DenialView is a refused device.
type DenialView struct {
	enroll.Denial
	Code string `json:"code"`
}

// Controller answers control requests.
type Controller struct {
	Srv *enroll.Server

	// Peers reads what the tunnel knows about its peers right now.
	Peers func() ([]PeerStatus, error)

	Now func() time.Time
	Log *slog.Logger
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Handle carries out one request.
func (c *Controller) Handle(req ControlRequest) ControlResponse {
	now := c.now()
	switch req.Op {
	case OpOverview:
		o := c.overview(now)
		return ControlResponse{OK: true, Overview: &o}
	case OpApprove:
		d, err := c.Srv.Approve(req.ID, now)
		return answer(d.Name, err)
	case OpDeny:
		r, err := c.Srv.Deny(req.ID, now)
		return answer(r.Name, err)
	case OpRevoke:
		d, err := c.Srv.Revoke(req.ID, now)
		return answer(d.Name, err)
	case OpAllow:
		d, err := c.Srv.Allow(req.ID, now)
		return answer(d.Name, err)
	case OpForget:
		return answer("", c.Srv.Forget(req.ID))
	case OpNotified:
		return answer("", c.Srv.MarkNotified(req.IDs))
	}
	return ControlResponse{Error: fmt.Sprintf("unknown operation %q", req.Op)}
}

func answer(name string, err error) ControlResponse {
	if err == nil {
		return ControlResponse{OK: true, Name: name}
	}
	r := ControlResponse{Error: err.Error()}
	var refused *enroll.RefusedError
	switch {
	case errors.As(err, &refused):
		r.Reason = refused.Reason
	case errors.Is(err, enroll.ErrNoRequest), errors.Is(err, enroll.ErrNoRefusal), errors.Is(err, enroll.ErrUnknownDevice):
		r.Missing = true
	}
	return r
}

func (c *Controller) overview(now time.Time) ControlOverview {
	o := c.Srv.Overview(now)
	live := map[string]PeerStatus{}
	if c.Peers != nil {
		if peers, err := c.Peers(); err == nil {
			for _, p := range peers {
				if k, err := awg.ParseKeyBase64(p.PublicKey); err == nil {
					live[enroll.KeyString(k)] = p
				}
			}
		} else {
			c.log().Warn("the overview goes without the tunnel's view of the devices", "error", err)
		}
	}

	out := ControlOverview{
		Approval: o.Approval, Service: o.Service,
		Devices: []DeviceView{}, Pending: []WaitingView{}, Denied: []DenialView{},
	}
	for _, d := range o.Devices {
		v := DeviceView{Device: d, Code: codeOf(d.PublicKey)}
		if p, ok := live[d.PublicKey]; ok {
			v.Online = p.Connected(now)
			if !p.LastHandshake.IsZero() {
				v.LastHandshake = p.LastHandshake.Unix()
			}
			v.RxBytes, v.TxBytes = p.RxBytes, p.TxBytes
		}
		out.Devices = append(out.Devices, v)
	}
	for _, w := range o.Pending {
		out.Pending = append(out.Pending, WaitingView{Waiting: w, Code: codeOf(w.PublicKey)})
	}
	for _, d := range o.Denied {
		out.Denied = append(out.Denied, DenialView{Denial: d, Code: codeOf(d.PublicKey)})
	}
	return out
}

func codeOf(pub string) string {
	raw, err := proto.Decode(pub, proto.TunnelKeySize)
	if err != nil {
		return "----"
	}
	return proto.ApprovalCode(raw)
}

// ServeControl answers requests on ln until the context ends: one request per
// connection, a line of JSON each way. It closes ln on the way out.
func (c *Controller) ServeControl(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go c.serveOne(conn)
	}
}

func (c *Controller) serveOne(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, maxControlMessage)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req ControlRequest
	resp := ControlResponse{Error: "the request is not valid JSON"}
	if json.Unmarshal(line, &req) == nil {
		resp = c.Handle(req)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(raw, '\n'))
}

// Control sends one request to the node's control socket and returns the
// answer. A response that says the operation failed is not an error here: the
// caller reads Error, Reason and Missing.
func Control(ctx context.Context, path string, req ControlRequest) (ControlResponse, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return ControlResponse{}, fmt.Errorf("the node's control socket: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return ControlResponse{}, err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return ControlResponse{}, fmt.Errorf("send to the node: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxControlMessage)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ControlResponse{}, fmt.Errorf("read the node's answer: %w", err)
	}
	var resp ControlResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return ControlResponse{}, fmt.Errorf("the node's answer is not valid JSON: %w", err)
	}
	return resp, nil
}
