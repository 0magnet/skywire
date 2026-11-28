package mute

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mute.json")
	s, err := OpenStore(path)
	require.NoError(t, err)
	require.True(t, s.Allows("g1"))
	require.NoError(t, s.SetMuted("g1", true))
	require.False(t, s.Allows("g1"))
	require.True(t, s.Allows("02aa"))
	require.NoError(t, s.SetEnabled(false))
	require.False(t, s.Allows("02aa"))
	require.ErrorIs(t, s.SetMuted("", true), ErrBadKey)

	s, err = OpenStore(path)
	require.NoError(t, err)
	require.False(t, s.Enabled())
	require.Equal(t, []string{"g1"}, s.List())
	require.NoError(t, s.SetMuted("g1", false))
	require.NoError(t, s.SetEnabled(true))
	require.True(t, s.Allows("g1"))

	var none *Store
	require.True(t, none.Allows("x"), "a missing store mutes nothing")
}
