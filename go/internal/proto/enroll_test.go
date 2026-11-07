package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func enrollFixture(t *testing.T, now time.Time) *EnrollRequest {
	t.Helper()
	req, err := NewEnrollRequest(repeat(0x07)[:DeviceIDSize], repeat(0x08)[:TunnelKeySize], "windows", now)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestEnrollRequestRoundTrip(t *testing.T) {
	now := time.Unix(testIat, 0)
	blob, err := enrollFixture(t, now).Seal(testContent)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenEnrollRequest(testContent, blob, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceID != Encode(repeat(0x07)[:DeviceIDSize]) || got.PublicKey != Encode(repeat(0x08)[:TunnelKeySize]) || got.Name != "windows" {
		t.Errorf("fields did not survive: %+v", got)
	}
}

func TestEnrollRequestNeedsTheContentKey(t *testing.T) {
	now := time.Unix(testIat, 0)
	blob, err := enrollFixture(t, now).Seal(testContent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEnrollRequest(repeat(0x44), blob, now); err == nil {
		t.Fatal("a request opened with the wrong content key")
	}
}

// A request that was recorded cannot be played back later. (It would only
// re-register the same key, but nothing needs to accept it.)
func TestEnrollRequestFreshness(t *testing.T) {
	issued := time.Unix(testIat, 0)
	blob, err := enrollFixture(t, issued).Seal(testContent)
	if err != nil {
		t.Fatal(err)
	}
	for name, now := range map[string]time.Time{
		"too old":    issued.Add(ClockSkew + time.Minute),
		"from ahead": issued.Add(-ClockSkew - time.Minute),
	} {
		if _, err := OpenEnrollRequest(testContent, blob, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, now := range map[string]time.Time{
		"a little late":  issued.Add(ClockSkew - time.Second),
		"a little early": issued.Add(-ClockSkew + time.Second),
	} {
		if _, err := OpenEnrollRequest(testContent, blob, now); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEnrollRequestShapeIsChecked(t *testing.T) {
	now := time.Unix(testIat, 0)
	good := func() *EnrollRequest { return enrollFixture(t, now) }

	cases := map[string]func(r *EnrollRequest){
		"short device id":  func(r *EnrollRequest) { r.DeviceID = Encode(make([]byte, 8)) },
		"short key":        func(r *EnrollRequest) { r.PublicKey = Encode(make([]byte, 31)) },
		"zero key":         func(r *EnrollRequest) { r.PublicKey = Encode(make([]byte, TunnelKeySize)) },
		"not base64":       func(r *EnrollRequest) { r.DeviceID = "!!!" },
		"short nonce":      func(r *EnrollRequest) { r.Nonce = Encode([]byte{1}) },
		"wrong version":    func(r *EnrollRequest) { r.V = 9 },
		"wrong kind":       func(r *EnrollRequest) { r.Kind = KindEnrolled },
		"name too long":    func(r *EnrollRequest) { r.Name = strings.Repeat("я", MaxDeviceName+1) },
		"padded base64url": func(r *EnrollRequest) { r.PublicKey += "=" },
	}
	for name, mutate := range cases {
		r := good()
		mutate(r)
		if _, err := r.Seal(testContent); err == nil {
			t.Errorf("%s: sealed", name)
		}
	}
}

// What is sealed is not checked by the receiver's own Seal, so the receiver has
// to check what arrives too.
func TestEnrollRequestIsCheckedOnArrival(t *testing.T) {
	now := time.Unix(testIat, 0)
	r := enrollFixture(t, now)
	r.PublicKey = Encode(make([]byte, TunnelKeySize))
	blob, err := Seal(testContent, mustJSON(t, r)) // bypasses r.Seal
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEnrollRequest(testContent, blob, now); err == nil {
		t.Fatal("a request with a zero key was accepted")
	}
}

func TestEnrollNameLosesControlCharacters(t *testing.T) {
	now := time.Unix(testIat, 0)
	r := enrollFixture(t, now)
	r.Name = "pho\x00ne\n\x1b[31m"
	blob, err := r.Seal(testContent)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenEnrollRequest(testContent, blob, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got.Name, "\x00\n\x1b") {
		t.Errorf("name kept control characters: %q", got.Name)
	}
}

// A document from the gist is encrypted under the same key. It must not be
// mistaken for a request.
func TestAGistDocumentIsNotARequest(t *testing.T) {
	now := time.Unix(testIat, 0)
	blob, err := Seal(testContent, []byte(`{"v":1,"kind":"config","seq":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEnrollRequest(testContent, blob, now); err == nil {
		t.Fatal("a config document was accepted as an enrollment request")
	}
}

func TestEnrollReplyIsBoundToItsRequest(t *testing.T) {
	now := time.Unix(testIat, 0)
	req := enrollFixture(t, now)

	blob, err := NewEnrolled(req, "10.8.0.9/32", now).Seal(testContent)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenEnrollResponse(testContent, blob, req.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != "10.8.0.9/32" || got.Error != "" {
		t.Errorf("reply = %+v", got)
	}
	if _, err := OpenEnrollResponse(testContent, blob, Encode(repeat(0x55)[:EnrollNonceSize])); err == nil {
		t.Error("a reply to another request was accepted")
	}
}

func TestEnrollRefusalIsReadable(t *testing.T) {
	now := time.Unix(testIat, 0)
	req := enrollFixture(t, now)
	blob, err := NewEnrollRefused(req.Nonce, EnrollFull, now).Seal(testContent)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenEnrollResponse(testContent, blob, req.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error != EnrollFull || got.Address != "" {
		t.Errorf("reply = %+v", got)
	}
}

func TestAnEmptyReplyIsRejected(t *testing.T) {
	now := time.Unix(testIat, 0)
	req := enrollFixture(t, now)
	blob, err := (&EnrollResponse{V: FormatVersion, Kind: KindEnrolled, Nonce: req.Nonce}).Seal(testContent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEnrollResponse(testContent, blob, req.Nonce); err == nil {
		t.Fatal("a reply that neither grants nor refuses was accepted")
	}
}

// A port reads the messages in the vectors and gets the documented fields.
func TestEnrollVectorsOpen(t *testing.T) {
	v := buildVectors(t)
	e := v.Enroll
	now := time.Unix(e.Now, 0)

	req, err := OpenEnrollRequest(testContent, e.RequestBlob, now)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if req.DeviceID != e.DeviceID || req.PublicKey != e.PublicKey || req.Nonce != e.RequestNonce || req.Name != e.Name {
		t.Errorf("request fields = %+v", req)
	}

	reply, err := OpenEnrollResponse(testContent, e.ReplyBlob, e.RequestNonce)
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if reply.Address != e.Address {
		t.Errorf("address = %q, want %q", reply.Address, e.Address)
	}

	refused, err := OpenEnrollResponse(testContent, e.RefusedBlob, e.RequestNonce)
	if err != nil {
		t.Fatalf("refusal: %v", err)
	}
	if refused.Error != EnrollFull {
		t.Errorf("refusal = %+v", refused)
	}
}
