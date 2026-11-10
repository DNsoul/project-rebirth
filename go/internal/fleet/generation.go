package fleet

import (
	"fmt"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
	"awgrotator/internal/node"
	"awgrotator/internal/proto"
)

// HealthClient is the tunnel address of the peer a server keeps for testing
// itself and for being tested by its predecessor. It is out of the way of the
// addresses handed to real clients.
const HealthClient = "10.8.0.250/32"

// Generation is everything created fresh for one server: its keys, its
// obfuscation parameters, its port, and the node configuration derived from
// them.
//
// A new server never reuses any of it, so two generations do not look alike on
// the wire, and a key that leaks from one says nothing about the next.
type Generation struct {
	Server awg.Keypair // the server's own tunnel key
	Enroll awg.Keypair // the enrollment peer, published to every device
	Health awg.Keypair // the test peer; its private half stays with whoever tests

	// SelfTest is the key the check before publication registers a device
	// with. The server lets it in without the operator, so its private half
	// stays with whoever tests, like Health's.
	SelfTest awg.Keypair

	Params awg.Params
	Port   int

	Node *node.Config
}

// GenOptions control how a generation is made.
type GenOptions struct {
	// Split turns on split routing.
	Split bool

	// ContentKey is what a registration request must be sealed with. The
	// server needs it to read one. Without it the server does not answer
	// registrations, which only experiments want.
	ContentKey []byte

	// Params overrides the generated obfuscation parameters. It exists for
	// experiments; deployments leave it nil.
	Params *awg.Params

	// Approval makes new devices wait for the operator on the new server.
	Approval bool
}

// NewGeneration makes a fresh set of keys and parameters.
func NewGeneration(opts GenOptions) (*Generation, error) {
	g := &Generation{}
	var err error

	for _, kp := range []*awg.Keypair{&g.Server, &g.Enroll, &g.Health, &g.SelfTest} {
		if *kp, err = awg.GenerateKeypair(); err != nil {
			return nil, err
		}
	}
	if g.Params, err = awg.GenerateParams(); err != nil {
		return nil, err
	}
	if opts.Params != nil {
		g.Params = *opts.Params
	}
	if err := g.Params.Validate(); err != nil {
		return nil, fmt.Errorf("obfuscation parameters: %w", err)
	}
	if g.Port, err = awg.RandomPort(); err != nil {
		return nil, err
	}

	g.Node = &node.Config{
		Version:    node.ConfigVersion,
		Interface:  "awg0",
		ListenPort: g.Port,
		PrivateKey: g.Server.Private,
		Params:     g.Params,
		Address:    ServerAddress,
		Subnet:     Subnet,
		Peers: []node.Peer{
			{PublicKey: g.Enroll.Public, AllowedIPs: []string{EnrollPeer}, Note: "служебный: регистрация устройств"},
			{PublicKey: g.Health.Public, AllowedIPs: []string{HealthClient}, Note: "проверка сервера"},
		},
	}
	if len(opts.ContentKey) > 0 {
		g.Node.Enroll = &node.EnrollConfig{
			Listen:      node.DefaultEnrollListen,
			PeerAddress: node.DefaultEnrollPeerAddress,
			ContentKey:  proto.Encode(opts.ContentKey),
			Approval:    opts.Approval,
			Preapproved: []string{enroll.KeyString(g.SelfTest.Public)},
		}
	}
	if opts.Split {
		g.Node.Split = node.NewSplitConfig()
	}
	if err := g.Node.Validate(); err != nil {
		return nil, fmt.Errorf("the new server's configuration: %w", err)
	}
	return g, nil
}

// ClientNet is what a client's configuration says about DNS and routes. Split
// routing and a plain full tunnel differ only here.
type ClientNet struct {
	DNS        []string
	AllowedIPs []string
}

// FullTunnel is the plain configuration: everything through the server.
var FullTunnel = ClientNet{DNS: DNS, AllowedIPs: []string{"0.0.0.0/0"}}

// Body assembles the configuration that gets published for this generation.
func (g *Generation) Body(region, ip string, net ClientNet, now time.Time) *ConfigBody {
	endpoint := fmt.Sprintf("%s:%d", ip, g.Port)
	return &ConfigBody{
		Version:         BodyVersion,
		Region:          region,
		Endpoint:        endpoint,
		CreatedAt:       now.Unix(),
		ServerPublicKey: g.Server.Public,
		Params:          g.Params,
		Subnet:          Subnet,
		DNS:             net.DNS,
		MTU:             awg.MTU,
		AllowedIPs:      net.AllowedIPs,
		Enroll: &EnrollInfo{
			PrivateKey: g.Enroll.Private,
			Address:    EnrollPeer,
			Server:     node.DefaultEnrollListen,
		},
	}
}
