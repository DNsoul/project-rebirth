package proto

import "encoding/binary"

// sigBuf builds the exact byte string that gets signed.
//
// Every field is written with an explicit length, so two independent
// implementations (Go here, Dart in the apps) always produce identical bytes
// without needing a canonical JSON encoder. See spec/format.md section 3.
type sigBuf struct {
	b []byte
}

// newSigBuf starts a buffer for the given document domain, e.g.
// "awg-rotator/cert/v1". The domain plus a NUL byte separates documents of
// different kinds so a signature can never be lifted from one to another.
func newSigBuf(domain string) *sigBuf {
	s := &sigBuf{}
	s.b = append(s.b, domain...)
	s.b = append(s.b, 0)
	return s
}

func (s *sigBuf) u32(v uint32) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	s.b = append(s.b, tmp[:]...)
}

func (s *sigBuf) u64(v uint64) {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	s.b = append(s.b, tmp[:]...)
}

func (s *sigBuf) i64(v int64) { s.u64(uint64(v)) }

// lp writes a length-prefixed field.
func (s *sigBuf) lp(v []byte) {
	s.u32(uint32(len(v)))
	s.b = append(s.b, v...)
}

// lps writes a string as a length-prefixed field of its UTF-8 bytes.
func (s *sigBuf) lps(v string) { s.lp([]byte(v)) }

func (s *sigBuf) bytes() []byte { return s.b }
