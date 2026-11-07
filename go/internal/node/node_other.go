//go:build !linux

package node

import (
	"context"
	"errors"
	"log/slog"
)

// errUnsupported keeps the package building on the development machine so the
// configuration and status code can be tested there. The node itself only
// runs on Linux, which is all the servers are.
var errUnsupported = errors.New("awgnode runs on linux only")

// Node is a stand-in on platforms that cannot host a tunnel.
type Node struct{}

// New returns a node that refuses to start.
func New(*Config, *slog.Logger) *Node { return &Node{} }

func (n *Node) Start(context.Context) error { return errUnsupported }
func (n *Node) Run(context.Context) error   { return errUnsupported }
func (n *Node) Close() error                { return nil }
func (n *Node) Status() (Status, error)     { return Status{}, errUnsupported }
func (n *Node) WriteStatus() error          { return errUnsupported }
