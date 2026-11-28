// Package visorapi pkg/visor/visorapi/rpc_tunnel.go c3-vis-core
package visorapi

import (
	"errors"

	"github.com/skycoin/skywire/pkg/cipher"
)

// ErrRPCTunnelClosed is returned by RPCTunnelRead and RPCTunnelWrite once
// the tunnel's far end has closed. Callers map it to io.EOF.
var ErrRPCTunnelClosed = errors.New("rpc tunnel closed")

// RPCTunnelReadArgs asks for up to Max bytes from tunnel ID.
type RPCTunnelReadArgs struct {
	ID  uint64
	Max int
}

// RPCTunnelWriteArgs sends Bytes into tunnel ID.
type RPCTunnelWriteArgs struct {
	ID    uint64
	Bytes []byte
}

// RPCTunnel carries a byte stream to a new RPC session on a visor over an
// existing RPC connection. A hypervisor uses it on its managed visors' conns,
// which is what `--via hv://<pk>` rides on.
type RPCTunnel interface {
	// RPCTunnelOpen opens a tunnel to a new RPC session on pk. pk is the
	// callee itself, or a visor attached to the callee's hypervisor.
	RPCTunnelOpen(pk cipher.PubKey) (uint64, error)
	// RPCTunnelRead waits a few seconds for bytes and returns none if
	// nothing arrived, so callers poll it in a loop.
	RPCTunnelRead(id uint64, limit int) ([]byte, error)
	RPCTunnelWrite(id uint64, b []byte) error
	RPCTunnelClose(id uint64) error
}

// RPCTunnelOpen implements RPCTunnel.
func (rc *rpcClient) RPCTunnelOpen(pk cipher.PubKey) (uint64, error) {
	var id uint64
	err := rc.Call("RPCTunnelOpen", &pk, &id)
	return id, err
}

// RPCTunnelRead implements RPCTunnel.
func (rc *rpcClient) RPCTunnelRead(id uint64, limit int) ([]byte, error) {
	var out []byte
	err := rc.Call("RPCTunnelRead", &RPCTunnelReadArgs{ID: id, Max: limit}, &out)
	return out, err
}

// RPCTunnelWrite implements RPCTunnel.
func (rc *rpcClient) RPCTunnelWrite(id uint64, b []byte) error {
	return rc.Call("RPCTunnelWrite", &RPCTunnelWriteArgs{ID: id, Bytes: b}, &struct{}{})
}

// RPCTunnelClose implements RPCTunnel.
func (rc *rpcClient) RPCTunnelClose(id uint64) error {
	return rc.Call("RPCTunnelClose", &id, &struct{}{})
}
