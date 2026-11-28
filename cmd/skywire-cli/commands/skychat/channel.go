// Package cliskychat cmd/skywire-cli/commands/skychat/channel.go c4-vis-cli
package cliskychat

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	clirpc "github.com/skycoin/skywire/cmd/skywire-cli/commands/rpc"
	"github.com/skycoin/skywire/pkg/skychat/mcpchannel"
)

var (
	channelGroups       []string
	channelDMs          bool
	channelSenders      []string
	channelIgnorePrefix string
	channelReplyPrefix  bool
	channelStatePath    string
)

// channelFlagsSetup registers the channel command's flags; root.go's init calls it.
func channelFlagsSetup() {
	channelCmd.Flags().StringVar(&clirpc.Addr, "rpc", clirpc.DefaultRPCAddr, "visor RPC address (with --via, the local visor that bridges to the remote one)")
	channelCmd.Flags().StringSliceVar(&channelGroups, "groups", nil, "group ids to deliver (default every group this visor is an active member of)")
	channelCmd.Flags().BoolVar(&channelDMs, "dms", true, "deliver direct messages too")
	channelCmd.Flags().StringSliceVar(&channelSenders, "senders", nil, "only deliver messages from these public keys (default every sender)")
	channelCmd.Flags().StringVar(&channelIgnorePrefix, "ignore-prefix", strings.TrimSpace(mcpchannel.BotPrefix), "drop inbound messages starting with this")
	channelCmd.Flags().BoolVar(&channelReplyPrefix, "prefix", true, "start every reply with \""+strings.TrimSpace(mcpchannel.BotPrefix)+"\"; turn off when the channel has a visor key of its own")
	channelCmd.Flags().StringVar(&channelStatePath, "state", "", "resume state file (default <user config dir>/skywire/skychat-channel/<local|pk>.json)")
}

var channelCmd = &cobra.Command{
	Use:   "channel",
	Short: "Serve skychat to Claude Code as a channel (MCP on stdio)",
	Long: `Serve skychat to Claude Code as an MCP channel on stdio.

Every inbound group message and DM is pushed into the Claude Code session as
a notifications/claude/channel event. Claude answers with the reply tool, and
can read earlier messages with history and list groups with groups.

The visor is reached over its RPC, so --via dmsg://<pk> or skynet://<pk> runs
the channel as that remote visor's identity. A dropped session reconnects
with backoff, and a state file resumes the stream without replaying history.

Load it in Claude Code from .mcp.json:

  {"mcpServers": {"skychat": {"command": "skywire",
    "args": ["cli", "skychat", "channel"]}}}

  claude --dangerously-load-development-channels server:skychat

Stdout carries the MCP protocol only; diagnostics go to stderr.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		statePath := channelStatePath
		if statePath == "" {
			statePath = channelStateDefault()
		}
		logf := func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...) //nolint:errcheck
		}
		srv := mcpchannel.NewServer(mcpchannel.Config{
			VisorDial: func() (mcpchannel.Visor, error) {
				return clirpc.ClientQuiet(cmd.Flags())
			},
			Groups:       channelGroups,
			DMs:          channelDMs,
			Senders:      channelSenders,
			IgnorePrefix: channelIgnorePrefix,
			ReplyPrefix:  channelReplyPrefix,
			StatePath:    statePath,
			Logf:         logf,
		})
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
			logf("skychat channel: %v", err)
			os.Exit(1)
		}
	},
}

// channelStateDefault names one state file per target visor.
func channelStateDefault() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	target := "local"
	for _, v := range []string{clirpc.Via, clirpc.VisorPK, clirpc.Addr} {
		if v == "" || (v == clirpc.Addr && !strings.Contains(v, "://")) {
			continue
		}
		target = strings.NewReplacer("dmsg://", "", "skynet://", "", "tp://", "", ":", "_", "/", "_").Replace(v)
		break
	}
	return filepath.Join(dir, "skywire", "skychat-channel", target+".json")
}
