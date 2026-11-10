package enroll

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"awgrotator/internal/proto"
)

// The operator's side of approval: what they see and what they decide. Every
// operation takes the same lock as an enrollment, so a decision and a request
// from the same device never cross.

// Errors an operation can end with.
var (
	ErrNoRequest    = errors.New("this device is not waiting")
	ErrNoRefusal    = errors.New("this device has not been refused")
	ErrNoAccessList = errors.New("this server keeps no waiting list")
)

// RefusedError is a decision the registry could not carry out: the key belongs
// to another device, or there is no room. Reason is one of the proto.Enroll*
// reasons.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "the device list refused it: " + e.Reason }

// SelfTestDeviceID is the identity a replacement server's own check registers
// under. It is the same every time, so every check replaces the previous one's
// entry and a server that is tested often still holds exactly one. It is
// derived from a fixed string, so it is no secret: what lets the check in
// without the operator is the preapproved key, not this.
func SelfTestDeviceID() []byte {
	sum := sha256.Sum256([]byte("awg-rotator/selftest/v1"))
	return sum[:proto.DeviceIDSize]
}

// Overview is what the operator is shown.
type Overview struct {
	// Approval says whether new devices wait for the operator.
	Approval bool `json:"approval"`

	// Devices are the approved devices, without the server's own check.
	Devices []Device `json:"devices"`

	// Service counts the entries left out of Devices.
	Service int `json:"service"`

	Pending []Waiting `json:"pending"`
	Denied  []Denial  `json:"denied"`
}

// Overview lists the devices, the waiting requests and the refusals.
func (s *Server) Overview(now time.Time) Overview {
	s.mu.Lock()
	defer s.mu.Unlock()

	o := Overview{Approval: s.Approval, Devices: []Device{}, Pending: []Waiting{}, Denied: []Denial{}}
	selfTest := proto.Encode(SelfTestDeviceID())
	for _, d := range s.Registry.Devices() {
		if d.ID == selfTest || s.preapproved(d.PublicKey) {
			o.Service++
			continue
		}
		o.Devices = append(o.Devices, d)
	}
	if s.Access != nil {
		o.Pending = s.Access.Pending(now)
		o.Denied = s.Access.Denied()
	}
	return o
}

// Approve lets a waiting device in.
func (s *Server) Approve(id string, now time.Time) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Access == nil {
		return Device{}, ErrNoAccessList
	}
	r, ok := s.Access.Request(id)
	if !ok {
		return Device{}, ErrNoRequest
	}
	d, err := s.admit(r.ID, r.PublicKey, r.Name, now)
	if err != nil {
		return Device{}, err
	}
	if err := s.Access.Settle(id, nil); err != nil {
		// The device is in; a request left behind only shows up once more.
		s.log().Warn("approved a device, but could not take it off the waiting list", "error", err)
	}
	s.log().Info("the operator approved a device", "name", d.Name, "address", d.Address)
	return d, nil
}

// Deny refuses a waiting device. A refused key change refuses only the new key:
// the device stays approved under the one it had.
func (s *Server) Deny(id string, now time.Time) (Waiting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Access == nil {
		return Waiting{}, ErrNoAccessList
	}
	r, ok := s.Access.Request(id)
	if !ok {
		return Waiting{}, ErrNoRequest
	}
	deny := &Denial{ID: r.ID, PublicKey: r.PublicKey, Name: r.Name, At: now.Unix(), KeyOnly: r.KeyChange}
	if err := s.Access.Settle(id, deny); err != nil {
		return Waiting{}, err
	}
	s.log().Info("the operator refused a device", "name", r.Name, "key_only", r.KeyChange)
	return r, nil
}

// Revoke cuts an approved device off: it leaves the list and the tunnel at
// once, and is refused if it asks again.
func (s *Server) Revoke(id string, now time.Time) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Access == nil {
		return Device{}, ErrNoAccessList
	}
	d, ok := s.Registry.Lookup(id)
	if !ok {
		return Device{}, ErrUnknownDevice
	}
	// The refusal first: if what follows fails half way, a refused device that
	// is still listed is turned away anyway (refusals win), whereas a device
	// taken off the list without a refusal would simply ask again.
	deny := &Denial{ID: d.ID, PublicKey: d.PublicKey, Name: d.Name, At: now.Unix()}
	if err := s.Access.Settle(id, deny); err != nil {
		return Device{}, err
	}
	if _, err := s.Registry.Remove(id); err != nil {
		return Device{}, fmt.Errorf("refused, but could not take it off the device list: %w", err)
	}
	if k, err := ParseKey(d.PublicKey); err == nil {
		if err := s.Peers.RemovePeer(k); err != nil {
			s.log().Warn("could not remove a revoked device from the tunnel", "error", err)
		}
	}
	s.log().Info("the operator revoked a device", "name", d.Name, "address", d.Address)
	return d, nil
}

// Allow lets a refused device in after all, with the key it last asked with.
func (s *Server) Allow(id string, now time.Time) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Access == nil {
		return Device{}, ErrNoAccessList
	}
	r, ok := s.Access.Denial(id)
	if !ok {
		return Device{}, ErrNoRefusal
	}
	d, err := s.admit(r.ID, r.PublicKey, r.Name, now)
	if err != nil {
		return Device{}, err
	}
	if err := s.Access.Forget(id); err != nil {
		// Left as it is, the refusal would turn the device away at once.
		return Device{}, fmt.Errorf("let in, but the refusal could not be removed: %w", err)
	}
	s.log().Info("the operator let a refused device in", "name", d.Name, "address", d.Address)
	return d, nil
}

// Forget removes a refusal without letting the device in: it may ask again.
func (s *Server) Forget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Access == nil {
		return ErrNoAccessList
	}
	if _, ok := s.Access.Denial(id); !ok {
		return ErrNoRefusal
	}
	return s.Access.Forget(id)
}

// MarkNotified records that the operator has been told about these requests.
func (s *Server) MarkNotified(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Access == nil {
		return ErrNoAccessList
	}
	return s.Access.MarkNotified(ids)
}

// admit registers a device: the tunnel first, the record second, then the keys
// it replaces are dropped. A device the tunnel accepts but the record does not
// know is forgotten at the next restart and enrols again; the other way round
// it would be told it is registered and fail.
func (s *Server) admit(id, pub, name string, now time.Time) (Device, error) {
	req := &proto.EnrollRequest{
		V: proto.FormatVersion, Kind: proto.KindEnroll,
		DeviceID: id, PublicKey: pub, Name: name,
	}
	plan, refusal := s.Registry.Plan(req, now)
	if refusal != "" {
		return Device{}, &RefusedError{Reason: refusal}
	}
	key, err := ParseKey(plan.Device.PublicKey)
	if err != nil {
		return Device{}, err
	}
	if err := s.Peers.SetPeer(key, plan.Device.Address); err != nil {
		return Device{}, fmt.Errorf("could not add it to the tunnel: %w", err)
	}
	if err := s.Registry.Commit(plan); err != nil {
		if !plan.Existing {
			_ = s.Peers.RemovePeer(key)
		}
		return Device{}, fmt.Errorf("could not save the device list: %w", err)
	}
	for _, old := range plan.Drop {
		if k, err := ParseKey(old); err == nil {
			if err := s.Peers.RemovePeer(k); err != nil {
				s.log().Warn("could not remove a replaced key from the tunnel", "error", err)
			}
		}
	}
	return plan.Device, nil
}
