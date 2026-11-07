package fleet

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"awgrotator/internal/awg"
)

// Run with -update to rewrite spec/conf-vectors.json from this implementation.
var updateConf = flag.Bool("update", false, "rewrite spec/conf-vectors.json")

const confVectorsPath = "../../../spec/conf-vectors.json"

// ConfVectors pins down the client configuration file: what the Dart port in
// the apps has to produce, byte for byte, from the same published body. A
// difference of one space is enough for a stock client to refuse the file, and
// for our own apps to disagree with awgctl about what a config looks like.
type ConfVectors struct {
	Note  string           `json:"note"`
	Cases []ConfVectorCase `json:"cases"`
}

// ConfVectorCase is one body, one device, and the file that comes out.
type ConfVectorCase struct {
	Name       string `json:"name"`
	Body       string `json:"body"`        // the published body, as it appears in the gist
	PrivateKey string `json:"private_key"` // the device's key, base64 as in a .conf
	PublicKey  string `json:"public_key"`  // what registering it sends: both ends must derive the same
	Address    string `json:"address"`
	Conf       string `json:"conf"`
}

func fixedKey(b byte) awg.Key {
	var k awg.Key
	for i := range k {
		k[i] = b
	}
	return k
}

func buildConfVectors(t *testing.T) ConfVectors {
	t.Helper()
	device := fixedKey(0x21)

	// Everything the generation can switch on: a header protection key, random
	// trailers and an I1 chain.
	full := &ConfigBody{
		Version:         BodyVersion,
		Region:          "sto",
		Endpoint:        "203.0.113.9:41820",
		CreatedAt:       1_762_400_000,
		ServerPublicKey: fixedKey(0x11),
		Params: awg.Params{
			Jc: 4, Jmin: 40, Jmax: 120, S1: 20, S2: 25, S3: 30, S4: 35,
			HeaderProtectionKey: fixedKey(0x12), RandomTrailers: true, I1: "<r 16>",
		},
		Subnet:     Subnet,
		DNS:        []string{"198.18.0.1"},
		MTU:        awg.MTU,
		AllowedIPs: []string{"198.18.0.0/15", "91.108.4.0/22", "149.154.160.0/20"},
		Enroll:     &EnrollInfo{PrivateKey: fixedKey(0x13), Address: EnrollPeer, Server: "10.8.0.1:7843"},
	}

	// And the bare minimum: nothing optional present.
	plain := &ConfigBody{
		Version:         BodyVersion,
		Region:          "waw",
		Endpoint:        "198.51.100.7:51820",
		CreatedAt:       1_762_400_000,
		ServerPublicKey: fixedKey(0x14),
		Params:          awg.Params{Jc: 3, Jmin: 50, Jmax: 500, S1: 12, S2: 12, S3: 12, S4: 12},
		Subnet:          Subnet,
		DNS:             []string{"1.1.1.1", "9.9.9.9"},
		MTU:             awg.MTU,
		AllowedIPs:      []string{"0.0.0.0/0"},
		Enroll:          &EnrollInfo{PrivateKey: fixedKey(0x15), Address: EnrollPeer, Server: "10.8.0.1:7843"},
	}

	devicePub, err := awg.PublicFrom(device)
	if err != nil {
		t.Fatal(err)
	}

	var v ConfVectors
	v.Note = "Контрольные векторы клиентского конфига для spec/format.md. Реализация на Dart " +
		"обязана из того же тела получить тот же текст до байта. Сгенерировано " +
		"go test ./internal/fleet -update."
	for _, c := range []struct {
		name string
		body *ConfigBody
		addr string
	}{{"split, every option", full, "10.8.0.7/32"}, {"full tunnel, nothing optional", plain, "10.8.0.30/32"}} {
		raw, err := c.body.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		v.Cases = append(v.Cases, ConfVectorCase{
			Name:       c.name,
			Body:       string(raw),
			PrivateKey: device.Base64(),
			PublicKey:  devicePub.Base64(),
			Address:    c.addr,
			Conf:       c.body.ClientConf(device, c.addr),
		})
	}
	return v
}

func TestConfVectorsMatchSpec(t *testing.T) {
	got := buildConfVectors(t)

	if *updateConf {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(confVectorsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(confVectorsPath, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", confVectorsPath)
		return
	}

	raw, err := os.ReadFile(confVectorsPath)
	if err != nil {
		t.Fatalf("read vectors (run with -update to create them): %v", err)
	}
	var want ConfVectors
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(got.Cases) != len(want.Cases) {
		t.Fatalf("%d cases, vectors have %d", len(got.Cases), len(want.Cases))
	}
	for i := range got.Cases {
		g, w := got.Cases[i], want.Cases[i]
		if g != w {
			t.Errorf("case %q changed\n got: %+v\nwant: %+v", g.Name, g, w)
		}
	}
}

// The body in each vector has to be readable by the same code the tools use, or
// the Dart side would be tested against input nothing ever produces.
func TestConfVectorBodiesParse(t *testing.T) {
	for _, c := range buildConfVectors(t).Cases {
		body, err := ParseBody([]byte(c.Body))
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		key, err := awg.ParseKeyBase64(c.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		if got := body.ClientConf(key, c.Address); got != c.Conf {
			t.Errorf("%s: the parsed body renders differently from the vector", c.Name)
		}
		if bytes.HasPrefix([]byte(c.Conf), []byte(awg.BOM)) {
			t.Errorf("%s: the file starts with a byte order mark", c.Name)
		}
	}
}
