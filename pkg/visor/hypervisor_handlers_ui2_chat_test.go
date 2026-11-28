//go:build !mobile

package visor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skycoin/skywire/pkg/visor/ui2"
)

func TestUI2ChatUnreadOnce(t *testing.T) {
	var s ui2ChatState
	ms := []ui2.ChatMsg{
		{ID: "1", TS: time.Now()},
		{ID: "2", TS: time.Now(), Outgoing: true},
		{ID: "3", TS: time.Now().Add(-30 * 24 * time.Hour)},
		{TS: time.Now()},
	}
	require.Equal(t, []string{"1"}, s.unread(ms))
	require.Empty(t, s.unread(ms), "a receipt goes out once")
}

func TestUI2ChatDataURI(t *testing.T) {
	mime, b, ok := ui2ChatDataURI("data:image/png;base64,aGk=")
	require.True(t, ok)
	require.Equal(t, "image/png", mime)
	require.Equal(t, "hi", string(b))
	_, _, ok = ui2ChatDataURI("data:text/html;base64,aGk=")
	require.False(t, ok)
	_, _, ok = ui2ChatDataURI("")
	require.False(t, ok)
}
