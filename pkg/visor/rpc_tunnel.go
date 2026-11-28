// Package visor pkg/visor/rpc_tunnel.go c3-vis-core
//
// RPC tunnels carry a byte stream to a new RPC session on a visor over an
// RPC connection that already exists. A hypervisor holds one RPC conn per
// managed visor, so the CLI's `--via hv://<pk>` reaches any visor attached to
// the local hypervisor, or to one of its sub-hypervisors, without a new dial
// and without a dmsgpty whitelist entry on the target. Each tunnel is a chain
// of RPCTunnelRead and RPCTunnelWrite calls, one link per hypervisor hop.
package visor

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/util/rpcutil"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

const (
	// rpcTunnelReadWait bounds one RPCTunnelRead. It stays well under the
	// hypervisor's 20 s RPC timeout so an idle tunnel never counts as a
	// timed out call on the conn it rides.
	rpcTunnelReadWait = 5 * time.Second
	// rpcTunnelChunkMax caps the bytes moved by one read or write call.
	rpcTunnelChunkMax = 64 << 10
	// rpcTunnelIdleTimeout closes a tunnel nobody has polled for this long,
	// which is what happens when the caller's conn dies without a close.
	rpcTunnelIdleTimeout = 2 * time.Minute
)

// rpcTunnel is one open tunnel held by the visor that answers
// RPCTunnelRead and RPCTunnelWrite for it.
type rpcTunnel struct {
	conn     io.ReadWriteCloser
	chunks   chan []byte
	done     chan struct{}
	doneOnce sync.Once
	lastUse  atomic.Int64

	readMu  sync.Mutex
	pending []byte
}

func newRPCTunnel(conn io.ReadWriteCloser) *rpcTunnel {
	t := &rpcTunnel{conn: conn, chunks: make(chan []byte, 16), done: make(chan struct{})}
	t.touch()
	go t.pump()
	return t
}

func (t *rpcTunnel) touch() { t.lastUse.Store(time.Now().UnixNano()) }

func (t *rpcTunnel) idleSince() time.Duration {
	return time.Since(time.Unix(0, t.lastUse.Load()))
}

func (t *rpcTunnel) pump() {
	defer close(t.chunks)
	for {
		buf := make([]byte, 32<<10)
		n, err := t.conn.Read(buf)
		if n > 0 {
			select {
			case t.chunks <- buf[:n]:
			case <-t.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (t *rpcTunnel) read(limit int, wait time.Duration) ([]byte, error) {
	t.touch()
	if limit <= 0 || limit > rpcTunnelChunkMax {
		limit = rpcTunnelChunkMax
	}
	t.readMu.Lock()
	defer t.readMu.Unlock()
	if len(t.pending) == 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case c, ok := <-t.chunks:
			if !ok {
				return nil, visorapi.ErrRPCTunnelClosed
			}
			t.pending = c
		case <-timer.C:
			return []byte{}, nil
		case <-t.done:
			return nil, visorapi.ErrRPCTunnelClosed
		}
	}
drain:
	for len(t.pending) < limit {
		select {
		case c, ok := <-t.chunks:
			if !ok {
				break drain
			}
			t.pending = append(t.pending, c...)
		default:
			break drain
		}
	}
	n := min(limit, len(t.pending))
	out := t.pending[:n:n]
	t.pending = t.pending[n:]
	return out, nil
}

func (t *rpcTunnel) write(b []byte) error {
	t.touch()
	select {
	case <-t.done:
		return visorapi.ErrRPCTunnelClosed
	default:
	}
	if _, err := t.conn.Write(b); err != nil {
		return visorapi.ErrRPCTunnelClosed
	}
	return nil
}

func (t *rpcTunnel) close() {
	t.doneOnce.Do(func() {
		close(t.done)
		_ = t.conn.Close() //nolint:errcheck,gosec
	})
}

// rpcTunnelTable holds a visor's open tunnels by ID. The zero value is ready.
type rpcTunnelTable struct {
	mu      sync.Mutex
	tunnels map[uint64]*rpcTunnel
	// readWait replaces rpcTunnelReadWait when set. Tests shorten it.
	readWait time.Duration
}

func (tt *rpcTunnelTable) add(conn io.ReadWriteCloser) uint64 {
	t := newRPCTunnel(conn)
	var b [8]byte
	tt.mu.Lock()
	if tt.tunnels == nil {
		tt.tunnels = make(map[uint64]*rpcTunnel)
	}
	var id uint64
	for id == 0 || tt.tunnels[id] != nil {
		_, _ = rand.Read(b[:]) //nolint:errcheck
		id = binary.LittleEndian.Uint64(b[:])
	}
	tt.tunnels[id] = t
	tt.mu.Unlock()
	go tt.reapWhenIdle(id, t)
	return id
}

func (tt *rpcTunnelTable) lookup(id uint64) (*rpcTunnel, error) {
	tt.mu.Lock()
	t := tt.tunnels[id]
	tt.mu.Unlock()
	if t == nil {
		return nil, visorapi.ErrRPCTunnelClosed
	}
	return t, nil
}

func (tt *rpcTunnelTable) remove(id uint64) {
	tt.mu.Lock()
	t := tt.tunnels[id]
	delete(tt.tunnels, id)
	tt.mu.Unlock()
	if t != nil {
		t.close()
	}
}

func (tt *rpcTunnelTable) reapWhenIdle(id uint64, t *rpcTunnel) {
	tick := time.NewTicker(rpcTunnelIdleTimeout / 4)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			tt.remove(id)
			return
		case <-tick.C:
			if t.idleSince() > rpcTunnelIdleTimeout {
				tt.remove(id)
				return
			}
		}
	}
}

// rpcTunnelConn is the caller's end of a tunnel held by a remote visor.
type rpcTunnelConn struct {
	api       visorapi.RPCTunnel
	id        uint64
	pending   []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newRPCTunnelConn(api visorapi.RPCTunnel, id uint64) *rpcTunnelConn {
	return &rpcTunnelConn{api: api, id: id, closed: make(chan struct{})}
}

func rpcTunnelErr(err error) error {
	if err != nil && strings.Contains(err.Error(), visorapi.ErrRPCTunnelClosed.Error()) {
		return io.EOF
	}
	return err
}

func (c *rpcTunnelConn) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		select {
		case <-c.closed:
			return 0, io.EOF
		default:
		}
		b, err := c.api.RPCTunnelRead(c.id, rpcTunnelChunkMax)
		if err != nil {
			return 0, rpcTunnelErr(err)
		}
		c.pending = b
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *rpcTunnelConn) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		end := min(len(p), written+rpcTunnelChunkMax)
		if err := c.api.RPCTunnelWrite(c.id, p[written:end]); err != nil {
			return written, rpcTunnelErr(err)
		}
		written = end
	}
	return written, nil
}

func (c *rpcTunnelConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		err = rpcTunnelErr(c.api.RPCTunnelClose(c.id))
		if errors.Is(err, io.EOF) {
			err = nil
		}
	})
	return err
}

// rpcTunnelDial opens a byte stream to a new RPC session on pk: this visor
// itself, a visor attached to this hypervisor, or one attached to a
// sub-hypervisor.
func (v *Visor) rpcTunnelDial(pk cipher.PubKey) (io.ReadWriteCloser, error) {
	if pk == v.conf.PK {
		rpcS, err := newRPCServer(v, "rpc_tunnel")
		if err != nil {
			return nil, err
		}
		local, served := net.Pipe()
		go rpcS.ServeConn(served)
		return local, nil
	}
	direct, sub, err := v.hvDispatch(pk)
	if err != nil {
		return nil, err
	}
	next := direct
	if next == nil {
		next = sub
	}
	tunnelAPI, ok := next.(visorapi.RPCTunnel)
	if !ok {
		return nil, fmt.Errorf("the connection to %s cannot carry an rpc tunnel", pk)
	}
	id, err := tunnelAPI.RPCTunnelOpen(pk)
	if err != nil {
		if strings.Contains(err.Error(), "RPCTunnelOpen") {
			return nil, fmt.Errorf("visor %s is too old for hv:// tunnels, update it or use --via dmsg://: %w", pk, err)
		}
		return nil, err
	}
	return newRPCTunnelConn(tunnelAPI, id), nil
}

// RPCTunnelOpen opens a tunnel to a new RPC session on pk.
func (r *RPC) RPCTunnelOpen(pk *cipher.PubKey, id *uint64) (err error) {
	defer rpcutil.LogCall(r.log, "RPCTunnelOpen", pk)(id, &err)
	v, ok := r.visor.(*Visor)
	if !ok {
		return fmt.Errorf("not a visor instance")
	}
	conn, err := v.rpcTunnelDial(*pk)
	if err != nil {
		return err
	}
	*id = v.rpcTunnels.add(conn)
	return nil
}

// RPCTunnelRead returns the next bytes from a tunnel, or none after a short wait.
func (r *RPC) RPCTunnelRead(in *visorapi.RPCTunnelReadArgs, out *[]byte) error {
	v, ok := r.visor.(*Visor)
	if !ok {
		return fmt.Errorf("not a visor instance")
	}
	t, err := v.rpcTunnels.lookup(in.ID)
	if err != nil {
		return err
	}
	wait := v.rpcTunnels.readWait
	if wait == 0 {
		wait = rpcTunnelReadWait
	}
	b, err := t.read(in.Max, wait)
	if err != nil {
		v.rpcTunnels.remove(in.ID)
		return err
	}
	*out = b
	return nil
}

// RPCTunnelWrite sends bytes into a tunnel.
func (r *RPC) RPCTunnelWrite(in *visorapi.RPCTunnelWriteArgs, _ *struct{}) error {
	v, ok := r.visor.(*Visor)
	if !ok {
		return fmt.Errorf("not a visor instance")
	}
	t, err := v.rpcTunnels.lookup(in.ID)
	if err != nil {
		return err
	}
	if err := t.write(in.Bytes); err != nil {
		v.rpcTunnels.remove(in.ID)
		return err
	}
	return nil
}

// RPCTunnelClose closes a tunnel.
func (r *RPC) RPCTunnelClose(id *uint64, _ *struct{}) (err error) {
	defer rpcutil.LogCall(r.log, "RPCTunnelClose", id)(nil, &err)
	v, ok := r.visor.(*Visor)
	if !ok {
		return fmt.Errorf("not a visor instance")
	}
	v.rpcTunnels.remove(*id)
	return nil
}
