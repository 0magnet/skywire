// Package cliskychat cmd/skywire-cli/commands/skychat/bridge.go c4-vis-cli
package cliskychat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	internal "github.com/skycoin/skywire/cmd/skywire-cli/cliutil"
	clirpc "github.com/skycoin/skywire/cmd/skywire-cli/commands/rpc"
	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/cmdutil"
	"github.com/skycoin/skywire/pkg/logging"
	"github.com/skycoin/skywire/pkg/skychat/bridge"
	"github.com/skycoin/skywire/pkg/visor/rpcgrpc"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

// Environment variables read by `bridge telegram`. The token is never a
// flag, since flags show in ps.
const (
	bridgeTelegramTokenEnv = "TG_BRIDGE_TOKEN" //nolint:gosec // the variable name, not a token
	bridgeTelegramChatEnv  = "TG_BRIDGE_CHAT"
	bridgeTelegramGroupEnv = "TG_BRIDGE_GROUP"
	bridgeTelegramStateEnv = "TG_BRIDGE_STATE"
)

var (
	bridgeTelegramChat       int64
	bridgeTelegramGroup      string
	bridgeTelegramState      string
	bridgeTelegramEnvFile    string
	bridgeTelegramSystemd    bool
	bridgeTelegramInterval   time.Duration
	bridgeSkychatInterval    time.Duration
	bridgeTelegramQueueLimit int
)

var bridgeCmd = &cobra.Command{
	Use:   "bridge",
	Short: "Relay a skychat group to another chat network",
}

var bridgeTelegramCmd = &cobra.Command{
	Use:   "telegram",
	Short: "Relay a skychat group to and from a Telegram chat",
	Long: `Relay messages between one skychat group and one Telegram chat.

Run it next to a visor that is a member of the group, normally the
group's host. Telegram lines are posted to the group as
"[TG] name: text" and group lines to Telegram as "[skychat] name: text".
Bot commands are not relayed.

The bot token is read from ` + bridgeTelegramTokenEnv + `, or from the file
given by --env-file, and never from a flag. --chat, --group and --state
default to ` + bridgeTelegramChatEnv + `, ` + bridgeTelegramGroupEnv + ` and ` + bridgeTelegramStateEnv + `.

The bot needs privacy mode off (BotFather /setprivacy) or admin rights
in the chat, or Telegram only shows it commands.

The state file keeps the Telegram offset, the newest group message
relayed and any lines not yet delivered, so a restart resumes without
reposting or skipping. --systemd prints a unit that keeps the token in
a root only EnvironmentFile.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		bridgeTelegramDefaults(cmd.Flags())
		if bridgeTelegramSystemd {
			cmdutil.Print(bridgeTelegramUnit())
			return
		}
		if bridgeTelegramChat == 0 || bridgeTelegramGroup == "" {
			internal.PrintFatalError(cmd.Flags(), errors.New("--chat and --group are required"))
		}
		token := os.Getenv(bridgeTelegramTokenEnv)
		if bridgeTelegramEnvFile != "" {
			fileToken, err := bridgeEnvFileToken(bridgeTelegramEnvFile)
			if err != nil {
				internal.PrintFatalError(cmd.Flags(), err)
			}
			if fileToken != "" {
				token = fileToken
			}
		}
		if token == "" {
			internal.PrintFatalError(cmd.Flags(), fmt.Errorf("no bot token: set %s or use --env-file", bridgeTelegramTokenEnv))
		}

		log := logging.MustGetLogger("skychat_bridge_telegram")
		b, err := bridge.TelegramNew(bridge.TelegramConfig{
			API:              &bridge.TelegramAPI{Token: token},
			ChatID:           bridgeTelegramChat,
			Group:            &bridgeGroupRPC{flags: cmd.Flags(), groupID: bridgeTelegramGroup},
			StatePath:        bridgeTelegramState,
			Log:              log,
			TelegramInterval: bridgeTelegramInterval,
			SkychatInterval:  bridgeSkychatInterval,
			QueueLimit:       bridgeTelegramQueueLimit,
		})
		if err != nil {
			internal.PrintFatalError(cmd.Flags(), err)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		log.Infof("bridging skychat group %s on %s", bridgeTelegramGroup, clirpc.Addr)
		b.Run(ctx)
	},
}

func bridgeTelegramFlags() {
	f := bridgeTelegramCmd.Flags()
	f.StringVar(&clirpc.Addr, "rpc", clirpc.DefaultRPCAddr, "RPC server address (env: SKYWIRE_RPC)")
	f.Int64Var(&bridgeTelegramChat, "chat", 0, "Telegram chat id, negative for a group (env "+bridgeTelegramChatEnv+")")
	f.StringVar(&bridgeTelegramGroup, "group", "", "skychat group id (env "+bridgeTelegramGroupEnv+")")
	f.StringVar(&bridgeTelegramState, "state", "skychat-telegram-bridge.json", "state file path (env "+bridgeTelegramStateEnv+")")
	f.StringVar(&bridgeTelegramEnvFile, "env-file", "", "file of KEY=value lines to read "+bridgeTelegramTokenEnv+" from")
	f.BoolVar(&bridgeTelegramSystemd, "systemd", false, "print a systemd unit for this bridge and exit")
	f.DurationVar(&bridgeTelegramInterval, "telegram-interval", 3*time.Second, "least time between messages to Telegram; lines waiting are joined")
	f.DurationVar(&bridgeSkychatInterval, "skychat-interval", time.Second, "least time between messages to the group; lines waiting are joined")
	f.IntVar(&bridgeTelegramQueueLimit, "queue-limit", 1000, "lines waiting per direction before the oldest are dropped and logged")
	bridgeCmd.AddCommand(bridgeTelegramCmd)
}

// bridgeTelegramDefaults fills flags the operator left unset from the
// environment.
func bridgeTelegramDefaults(f *pflag.FlagSet) {
	if v := os.Getenv(bridgeTelegramChatEnv); v != "" && !f.Changed("chat") {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			bridgeTelegramChat = id
		}
	}
	if v := os.Getenv(bridgeTelegramGroupEnv); v != "" && !f.Changed("group") {
		bridgeTelegramGroup = v
	}
	if v := os.Getenv(bridgeTelegramStateEnv); v != "" && !f.Changed("state") {
		bridgeTelegramState = v
	}
}

// bridgeEnvFileToken reads the token from a systemd style env file.
func bridgeEnvFileToken(path string) (string, error) {
	fh, err := os.Open(path) //nolint:gosec // operator supplied path
	if err != nil {
		return "", err
	}
	defer fh.Close() //nolint:errcheck
	if st, err := fh.Stat(); err == nil && st.Mode().Perm()&0o077 != 0 {
		cmdutil.Eprintf("warning: %s is readable by other users, chmod 600 it\n", path) //nolint:errcheck
	}
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(key, "export ")) == bridgeTelegramTokenEnv {
			return strings.Trim(strings.TrimSpace(value), `"'`), nil
		}
	}
	return "", sc.Err()
}

func bridgeTelegramUnit() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "/opt/skywire/skywire"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	chat := "<telegram-chat-id>"
	if bridgeTelegramChat != 0 {
		chat = strconv.FormatInt(bridgeTelegramChat, 10)
	}
	group := bridgeTelegramGroup
	if group == "" {
		group = "<skychat-group-id>"
	}
	return fmt.Sprintf(`# /etc/systemd/system/skychat-telegram-bridge.service
# Put the token in /etc/skywire/telegram-bridge.env as
#   %s=<bot token>
# owned by root with mode 600. systemd reads it before starting the bridge.
[Unit]
Description=Skychat Telegram bridge
After=network-online.target skywire.service
Wants=network-online.target

[Service]
EnvironmentFile=/etc/skywire/telegram-bridge.env
ExecStart=%s cli skychat bridge telegram --rpc %s --chat %s --group %s --state /var/lib/skychat-telegram-bridge/state.json
DynamicUser=yes
StateDirectory=skychat-telegram-bridge
StateDirectoryMode=0700
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`, bridgeTelegramTokenEnv, exe, clirpc.Addr, chat, group)
}

// bridgeGroupRPC is a skychat group reached over the visor RPC, the same
// calls `group send` and `group listen` make. A failed call drops the
// connection so the next one dials again after a visor restart.
type bridgeGroupRPC struct {
	flags   *pflag.FlagSet
	groupID string

	mu  sync.Mutex
	api visorapi.API
}

func (g *bridgeGroupRPC) client() (visorapi.API, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.api != nil {
		return g.api, nil
	}
	api, err := clirpc.ClientQuiet(g.flags)
	if err != nil {
		return nil, err
	}
	g.api = api
	return api, nil
}

func (g *bridgeGroupRPC) reset(api visorapi.API) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.api == api {
		_ = api.Close() //nolint:errcheck
		g.api = nil
	}
}

func (g *bridgeGroupRPC) Send(_ context.Context, text string) error {
	api, err := g.client()
	if err != nil {
		return err
	}
	if err := api.GroupSend(visorapi.GroupSendArgs{ID: g.groupID, Text: text}); err != nil {
		g.reset(api)
		return err
	}
	return nil
}

func (g *bridgeGroupRPC) Stream(ctx context.Context, sinceNs int64, deliver func(bridge.GroupMessage)) error {
	c, err := rpcgrpc.NewPingClient(clirpc.Addr)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck
	return c.StreamGroupMessages(ctx, g.groupID, sinceNs, nil, func(evt *rpcgrpc.GroupMessageEvent) {
		if evt.GroupId != g.groupID {
			return
		}
		deliver(bridge.GroupMessage{TimestampNs: evt.TimestampNs, SenderPK: evt.SenderPk, Body: evt.Body})
	})
}

func (g *bridgeGroupRPC) SelfPK(_ context.Context) (string, error) {
	api, err := g.client()
	if err != nil {
		return "", err
	}
	p, err := api.ProfileGet()
	if err != nil {
		g.reset(api)
		return "", err
	}
	return p.PK.Hex(), nil
}

func (g *bridgeGroupRPC) ProfileName(_ context.Context, pk string) (string, error) {
	var key cipher.PubKey
	if err := key.Set(pk); err != nil {
		return "", err
	}
	api, err := g.client()
	if err != nil {
		return "", err
	}
	p, err := api.ProfileFetch(key)
	if err != nil {
		return "", err
	}
	return p.Name, nil
}
