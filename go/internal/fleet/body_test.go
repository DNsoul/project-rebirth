package fleet

import (
	"strings"
	"testing"
	"time"

	"awgrotator/internal/awg"
)

func validBody(t *testing.T) *ConfigBody {
	t.Helper()
	server, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	enroll, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	params, err := awg.GenerateParams()
	if err != nil {
		t.Fatal(err)
	}
	return &ConfigBody{
		Version:         BodyVersion,
		Region:          "sto",
		Endpoint:        "203.0.113.9:41820",
		CreatedAt:       time.Now().Unix(),
		ServerPublicKey: server.Public,
		Params:          params,
		Subnet:          Subnet,
		DNS:             DNS,
		MTU:             awg.MTU,
		AllowedIPs:      []string{"0.0.0.0/0"},
		Enroll: &EnrollInfo{
			PrivateKey: enroll.Private,
			Address:    EnrollPeer,
			Server:     "10.8.0.1:7843",
		},
	}
}

func TestBodyRoundTrip(t *testing.T) {
	body := validBody(t)
	raw, err := body.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := ParseBody(raw)
	if err != nil {
		t.Fatalf("ParseBody: %v", err)
	}
	if got.Endpoint != body.Endpoint || got.Region != body.Region {
		t.Errorf("metadata changed: %+v", got)
	}
	if got.ServerPublicKey != body.ServerPublicKey {
		t.Error("server key did not survive the round trip")
	}
	if got.Params != body.Params {
		t.Errorf("parameters changed:\n got %+v\nwant %+v", got.Params, body.Params)
	}
	if got.Enroll == nil || *got.Enroll != *body.Enroll {
		t.Errorf("enrollment info did not survive the round trip: %+v", got.Enroll)
	}
	if got.Shared != nil {
		t.Error("a version 2 body grew a shared client")
	}
}

func TestBodyStoresKeysReadably(t *testing.T) {
	body := validBody(t)
	raw, err := body.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), body.ServerPublicKey.Base64()) {
		t.Errorf("server key is not stored as base64:\n%s", raw)
	}
}

func TestBodyValidateCatchesBadBodies(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*ConfigBody)
		want   string
	}{
		{"wrong version", func(b *ConfigBody) { b.Version = 7 }, "version"},
		{"version zero", func(b *ConfigBody) { b.Version = 0 }, "version"},
		{"no endpoint", func(b *ConfigBody) { b.Endpoint = "" }, "endpoint"},
		{"no server key", func(b *ConfigBody) { b.ServerPublicKey = awg.Key{} }, "public key"},
		{"no allowed addresses", func(b *ConfigBody) { b.AllowedIPs = nil }, "allowed"},
		{"bad parameters", func(b *ConfigBody) { b.Params.S3 = 2 }, "S3"},
		{"no way to register", func(b *ConfigBody) { b.Enroll = nil }, "register"},
		{"enrollment without a key", func(b *ConfigBody) { b.Enroll.PrivateKey = awg.Key{} }, "register"},
		{"enrollment without an address", func(b *ConfigBody) { b.Enroll.Server = "" }, "register"},
	}
	for _, tc := range cases {
		body := validBody(t)
		tc.break_(body)
		err := body.Validate()
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

// The server that is running when this build is first deployed still publishes
// version 1, and the tools must keep working against it until it is replaced.
func TestVersionOneBodiesAreStillRead(t *testing.T) {
	body := validBody(t)
	body.Version = 1
	body.Enroll = nil
	body.Shared = &SharedClient{Address: "10.8.0.2/32", Conf: "[Interface]\n"}
	raw, err := body.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseBody(raw)
	if err != nil {
		t.Fatalf("a version 1 body was refused: %v", err)
	}
	if got.Shared == nil {
		t.Error("the shared client of a version 1 body was lost")
	}
}

func TestParseBodyRejectsGarbage(t *testing.T) {
	if _, err := ParseBody([]byte("not json")); err == nil {
		t.Error("garbage parsed as a body")
	}
	if _, err := ParseBody([]byte("{}")); err == nil {
		t.Error("an empty object parsed as a body")
	}
}

func TestAddressingIsConsistent(t *testing.T) {
	// Everything that gets an address must sit inside the subnet that is
	// masqueraded, and no two of them may share one.
	addrs := []string{ServerAddress, EnrollPeer, HealthClient}
	for _, addr := range addrs {
		if !strings.HasPrefix(addr, "10.8.0.") {
			t.Errorf("%s is outside %s", addr, Subnet)
		}
	}
	seen := map[string]bool{}
	for _, addr := range addrs {
		host := strings.SplitN(addr, "/", 2)[0]
		if seen[host] {
			t.Errorf("%s is used twice", host)
		}
		seen[host] = true
	}
}
