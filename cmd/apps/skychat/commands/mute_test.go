package commands

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNotifySettingsHandler(t *testing.T) {
	old := muteStore
	t.Cleanup(func() { muteStore = old })
	openMuteStore(filepath.Join(t.TempDir(), "mute.json"))
	require.NotNil(t, muteStore)

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		notifySettingsHandler(w, httptest.NewRequest(http.MethodPost, "/notify-settings", strings.NewReader(body)))
		return w
	}
	w := post(`{"key":"g1","muted":true}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"muted":["g1"]`)
	require.False(t, muteStore.Allows("g1"))
	w = post(`{"enabled":false}`)
	require.Contains(t, w.Body.String(), `"enabled":false`)
	require.False(t, muteStore.Allows("02aa"))
	require.Equal(t, http.StatusBadRequest, post(`{}`).Code)
}
