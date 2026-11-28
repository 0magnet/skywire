package visor

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/contacts"
	"github.com/skycoin/skywire/pkg/logging"
	"github.com/skycoin/skywire/pkg/skyenv"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
	"github.com/skycoin/skywire/pkg/visor/visorconfig"
)

func rpcTunnelTestVisor(t *testing.T) (*Visor, cipher.PubKey) {
	t.Helper()
	pk, sk := cipher.GenerateKeyPair()
	common, err := visorconfig.NewCommon(nil, "", &sk)
	require.NoError(t, err)
	v := &Visor{conf: &visorconfig.V1{Common: common}}
	v.conf.LocalPath = filepath.Join(t.TempDir(), "local")
	return v, pk
}

// rpcTunnelTestAttach serves managed's RPC over a pipe the way ServeRPCClient
// does on a hypervisor conn, and returns the hypervisor's client for it.
func rpcTunnelTestAttach(t *testing.T, managed *Visor) visorapi.API {
	t.Helper()
	rpcS, err := newRPCServer(managed, "test-hv")
	require.NoError(t, err)
	hvEnd, visorEnd := net.Pipe()
	go rpcS.ServeConn(visorEnd)
	t.Cleanup(func() { _ = hvEnd.Close() }) //nolint:errcheck
	return visorapi.NewRPCClient(nil, hvEnd, visorapi.RPCPrefix, skyenv.RPCTimeout)
}

func rpcTunnelTestHypervisor(v *Visor, remotes map[cipher.PubKey]Conn) {
	v.hvInstance = &Hypervisor{
		c:            visorconfig.HypervisorConfig{PK: v.conf.PK},
		visor:        v,
		mu:           new(sync.RWMutex),
		remoteVisors: remotes,
	}
}

// rpcTunnelSubAPI is a sub-hypervisor's conn whose HVListVisors answers from
// a fixed list, so the test does not need a full visor behind Summary.
type rpcTunnelSubAPI struct {
	visorapi.API
	visorapi.RPCTunnel
	listed []visorapi.HVVisorEntry
}

func (s rpcTunnelSubAPI) HVListVisors() ([]visorapi.HVVisorEntry, error) { return s.listed, nil }

// rpcTunnelTestBridge does what `--via hv://<pk>` does: a bridge header with
// scheme 2 on the local visor's RPC port, then a plain RPC client.
func rpcTunnelTestBridge(t *testing.T, local *Visor, target cipher.PubKey) visorapi.API {
	t.Helper()
	cliEnd, visorEnd := net.Pipe()
	go handleBridgeConn(context.Background(), logging.MustGetLogger("rpc-tunnel-test"), visorEnd, local)
	header := make([]byte, bridgeHeaderSize)
	copy(header, bridgeMagic)
	header[6] = bridgeSchemeHypervisor
	copy(header[7:40], target[:])
	binary.LittleEndian.PutUint16(header[40:], skyenv.DmsgVisorRPCPort)
	_, err := cliEnd.Write(header)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cliEnd.Close() }) //nolint:errcheck
	return visorapi.NewRPCClient(nil, cliEnd, visorapi.RPCPrefix, 10*time.Second)
}

// TestRPCTunnelThroughHypervisors reaches the hypervisor itself, a directly
// attached visor and a visor behind a sub-hypervisor, each with the normal
// RPC client over one bridge conn.
func TestRPCTunnelThroughHypervisors(t *testing.T) {
	top, topPK := rpcTunnelTestVisor(t)
	direct, directPK := rpcTunnelTestVisor(t)
	sub, subPK := rpcTunnelTestVisor(t)
	leaf, leafPK := rpcTunnelTestVisor(t)

	rpcTunnelTestHypervisor(sub, map[cipher.PubKey]Conn{leafPK: {API: rpcTunnelTestAttach(t, leaf)}})
	subAPI := rpcTunnelTestAttach(t, sub)
	rpcTunnelTestHypervisor(top, map[cipher.PubKey]Conn{
		directPK: {API: rpcTunnelTestAttach(t, direct)},
		subPK: {API: rpcTunnelSubAPI{
			API:       subAPI,
			RPCTunnel: subAPI.(visorapi.RPCTunnel),
			listed:    []visorapi.HVVisorEntry{{PK: subPK}, {PK: leafPK}},
		}},
	})

	for _, tc := range []struct {
		name string
		pk   cipher.PubKey
		v    *Visor
	}{
		{"self", topPK, top},
		{"direct", directPK, direct},
		{"sub-hypervisor", subPK, sub},
		{"behind sub-hypervisor", leafPK, leaf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := rpcTunnelTestBridge(t, top, tc.pk)
			_, err := api.ContactsPut(contacts.Contact{Name: "via " + tc.name})
			require.NoError(t, err)
			got, err := tc.v.ContactsList()
			require.NoError(t, err)
			require.Len(t, got, 1, "the call ran on the target visor")
			require.Equal(t, "via "+tc.name, got[0].Name)
			listed, err := api.ContactsList()
			require.NoError(t, err)
			require.Equal(t, got, listed)
		})
	}

	// Each visor holds only its own contact, so no call landed on a hop.
	for _, v := range []*Visor{top, direct, sub, leaf} {
		got, err := v.ContactsList()
		require.NoError(t, err)
		require.Len(t, got, 1)
	}

	t.Run("not attached", func(t *testing.T) {
		stranger, _ := cipher.GenerateKeyPair()
		_, err := rpcTunnelTestBridge(t, top, stranger).ContactsList()
		require.Error(t, err)
	})
}

// TestRPCTunnelStreaming moves bytes that arrive slower than one
// RPCTunnelRead waits, and a payload many chunks long, through a tunnel.
func TestRPCTunnelStreaming(t *testing.T) {
	v, _ := rpcTunnelTestVisor(t)
	v.rpcTunnels.readWait = 20 * time.Millisecond
	api := rpcTunnelTestAttach(t, v)
	held, far := net.Pipe()
	id := v.rpcTunnels.add(held)
	conn := newRPCTunnelConn(api.(visorapi.RPCTunnel), id)

	// Slow output, as from a follow-mode log: each piece comes after
	// several empty polls.
	go func() {
		for i := range 5 {
			time.Sleep(60 * time.Millisecond)
			_, _ = far.Write([]byte{byte('a' + i)}) //nolint:errcheck
		}
	}()
	got := make([]byte, 0, 5)
	buf := make([]byte, 8)
	for len(got) < 5 {
		n, err := conn.Read(buf)
		require.NoError(t, err)
		got = append(got, buf[:n]...)
	}
	require.Equal(t, "abcde", string(got))

	// A payload larger than one chunk, echoed back.
	go func() { _, _ = io.Copy(far, far) }() //nolint:errcheck
	payload := bytes.Repeat([]byte("0123456789abcdef"), 3*rpcTunnelChunkMax/16+7)
	readBack := make(chan []byte, 1)
	go func() {
		b := make([]byte, len(payload))
		_, _ = io.ReadFull(conn, b) //nolint:errcheck
		readBack <- b
	}()
	n, err := conn.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	select {
	case b := <-readBack:
		require.Equal(t, payload, b)
	case <-time.After(10 * time.Second):
		t.Fatal("echo did not come back")
	}

	// Closing the far end ends the tunnel with EOF.
	require.NoError(t, far.Close())
	for {
		_, err = conn.Read(buf)
		if err != nil {
			break
		}
	}
	require.ErrorIs(t, err, io.EOF)
	_, err = v.rpcTunnels.lookup(id)
	require.Error(t, err, "a finished tunnel leaves the table")
	require.NoError(t, conn.Close())
}

// rpcTunnelOldAPI is a managed visor built before RPC tunnels.
type rpcTunnelOldAPI struct {
	visorapi.API
	visorapi.RPCTunnel
}

func (rpcTunnelOldAPI) RPCTunnelOpen(cipher.PubKey) (uint64, error) {
	return 0, errors.New("gobrpc: can't find method app-visor.RPCTunnelOpen")
}

func TestRPCTunnelOldVisor(t *testing.T) {
	top, _ := rpcTunnelTestVisor(t)
	oldPK, _ := cipher.GenerateKeyPair()
	rpcTunnelTestHypervisor(top, map[cipher.PubKey]Conn{oldPK: {API: rpcTunnelOldAPI{}}})
	_, err := top.rpcTunnelDial(oldPK)
	require.ErrorContains(t, err, "too old for hv://")
}
