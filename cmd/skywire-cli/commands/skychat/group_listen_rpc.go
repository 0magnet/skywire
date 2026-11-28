// Package cliskychat cmd/skywire-cli/commands/skychat/group_listen_rpc.go c4-vis-cli
package cliskychat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	clirpc "github.com/skycoin/skywire/cmd/skywire-cli/commands/rpc"
	"github.com/skycoin/skywire/pkg/cmdutil"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

// groupInbox is the part of the visor RPC that `group listen` follows.
type groupInbox interface {
	GroupPoll(since time.Time) ([]visorapi.GroupMessage, error)
	GroupWait(since time.Time, wait time.Duration) ([]visorapi.GroupMessage, error)
}

// groupListenWait is one GroupWait long-poll. The visor caps it at 20 s.
const groupListenWait = 20 * time.Second

func groupListenRPC(ctx context.Context, cmd *cobra.Command, since time.Time, onMsg func(tsNs int64, groupID, senderPK, body string)) {
	wait := groupListenWait
	if clirpc.Timeout > 0 {
		wait = min(wait, time.Duration(clirpc.Timeout)*time.Second/2)
	}
	dial := func() (groupInbox, error) { return clirpc.ClientQuiet(cmd.Flags()) }
	groupInboxFollow(ctx, dial, since, wait, groupListenPoll, func(m visorapi.GroupMessage) {
		onMsg(m.TS.UnixNano(), m.GroupID, m.SenderPK.Hex(), m.Text)
	})
}

// groupInboxFollow prints the inbox once, keeping messages newer than since,
// then every later arrival. It redials after errors. A visor without
// GroupWait is polled every interval instead.
func groupInboxFollow(ctx context.Context, dial func() (groupInbox, error), since time.Time, wait, interval time.Duration, emit func(visorapi.GroupMessage)) {
	const (
		minBackoff = time.Second
		maxBackoff = 30 * time.Second
	)
	var (
		inbox    groupInbox
		arrived  time.Time
		started  bool
		poll     bool
		failing  bool
		backoff  = minBackoff
		printed  = newIDDedupe(20000)
		sleepFor = func(d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		}
	)
	for ctx.Err() == nil {
		var err error
		if inbox == nil {
			inbox, err = dial()
		}
		var msgs []visorapi.GroupMessage
		if err == nil {
			switch {
			case !started:
				msgs, err = inbox.GroupPoll(time.Time{})
			case poll:
				msgs, err = inbox.GroupPoll(arrived)
			default:
				msgs, err = inbox.GroupWait(arrived, wait)
				if err != nil && strings.Contains(err.Error(), "GroupWait") {
					poll, err = true, nil
					continue
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !failing {
				cmdutil.Eprintf("group listen: %v (reconnecting in %s)\n", err, backoff) //nolint:errcheck
				failing = true
			}
			inbox = nil
			if !sleepFor(backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		if failing {
			cmdutil.Eprintln("group listen: reconnected") //nolint:errcheck
			failing = false
			backoff = minBackoff
		}
		for _, m := range msgs {
			if m.Arrived.After(arrived) {
				arrived = m.Arrived
			}
			// A restarted visor refills its inbox with new arrival times.
			key := fmt.Sprintf("%s|%s|%d", m.GroupID, m.SenderPK.Hex(), m.TS.UnixNano())
			if printed.has(key) || (!started && !m.TS.After(since)) {
				printed.add(key)
				continue
			}
			printed.add(key)
			emit(m)
		}
		started = true
		if poll && !sleepFor(interval) {
			return
		}
	}
}
