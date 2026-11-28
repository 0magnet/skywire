package clirpc

import (
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skycoin/skywire/pkg/cipher"
)

// TestViaClientHypervisor sends the bridge header with scheme 2 for both
// spellings of the hv scheme.
func TestViaClientHypervisor(t *testing.T) {
	pk, _ := cipher.GenerateKeyPair()
	for _, via := range []string{"hv://" + pk.Hex(), "hv:" + pk.Hex()} {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		headers := make(chan []byte, 1)
		go func() {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			defer conn.Close() //nolint:errcheck
			b := make([]byte, 42)
			_, _ = io.ReadFull(conn, b) //nolint:errcheck
			headers <- b
		}()
		Addr = lis.Addr().String()
		_, err = viaClient(nil, via, true)
		require.NoError(t, err, via)
		h := <-headers
		require.Equal(t, dmsgBridgeMagic, string(h[:6]))
		require.Equal(t, byte(2), h[6], via)
		require.Equal(t, pk[:], h[7:40], via)
		require.NoError(t, lis.Close())
	}
}
