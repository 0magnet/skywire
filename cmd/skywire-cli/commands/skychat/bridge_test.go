// Package cliskychat cmd/skywire-cli/commands/skychat/bridge_test.go c4-vis-cli
package cliskychat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBridgeEnvFileToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.env")
	body := "# comment\nOTHER=1\nexport TG_BRIDGE_TOKEN=\"123:abc\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := bridgeEnvFileToken(path)
	if err != nil || got != "123:abc" {
		t.Fatalf("token %q, %v", got, err)
	}
}

func TestBridgeTelegramUnit(t *testing.T) {
	bridgeTelegramChat, bridgeTelegramGroup = -1001, "g1"
	defer func() { bridgeTelegramChat, bridgeTelegramGroup = 0, "" }()
	t.Setenv(bridgeTelegramTokenEnv, "123:must-not-appear")
	unit := bridgeTelegramUnit()
	for _, want := range []string{"EnvironmentFile=/etc/skywire/telegram-bridge.env", "--chat -1001 --group g1", "cli skychat bridge telegram"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "must-not-appear") {
		t.Error("unit contains the token")
	}
}
