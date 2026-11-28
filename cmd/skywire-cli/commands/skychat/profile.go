// Package cliskychat cmd/skywire-cli/commands/skychat/profile.go c4-vis-cli
package cliskychat

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	internal "github.com/skycoin/skywire/cmd/skywire-cli/cliutil"
	clirpc "github.com/skycoin/skywire/cmd/skywire-cli/commands/rpc"
	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

var (
	profileName        string
	profileAvatar      string
	profileClearAvatar bool
	profileClear       bool
)

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Show, set or fetch a skychat profile: display name and picture",
}

var profileShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show this visor's skychat profile",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		rpcClient, err := clirpc.Client(cmd.Flags())
		if err != nil {
			internal.PrintFatalError(cmd.Flags(), err)
		}
		p, err := rpcClient.ProfileGet()
		if err != nil {
			internal.PrintFatalError(cmd.Flags(), err)
		}
		internal.PrintOutput(cmd.Flags(), p, profileText(p))
	},
}

var profileSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set this visor's skychat name and picture",
	Long: `Set this visor's skychat display name and picture. A flag left out keeps
the current value. The picture is an image file of at most 256x256 pixels
and 32 KiB; --clear-avatar removes it and --clear removes the whole profile.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		rpcClient, err := clirpc.Client(cmd.Flags())
		if err != nil {
			internal.PrintFatalError(cmd.Flags(), err)
		}
		args := visorapi.ProfileSetArgs{Clear: profileClear}
		if !profileClear {
			cur, err := rpcClient.ProfileGet()
			if err != nil {
				internal.PrintFatalError(cmd.Flags(), err)
			}
			args.Name, args.Avatar = cur.Name, cur.Avatar
			if cmd.Flags().Changed("name") {
				args.Name = profileName
			}
			switch {
			case profileClearAvatar && profileAvatar != "":
				internal.PrintFatalError(cmd.Flags(), fmt.Errorf("give --avatar or --clear-avatar, not both"))
			case profileClearAvatar:
				args.Avatar = ""
			case profileAvatar != "":
				data, err := os.ReadFile(profileAvatar) //nolint:gosec // the operator names the file
				if err != nil {
					internal.PrintFatalError(cmd.Flags(), err)
				}
				args.Avatar = "data:" + http.DetectContentType(data) + ";base64," + base64.StdEncoding.EncodeToString(data)
			}
		}
		p, err := rpcClient.ProfileSet(args)
		if err != nil {
			internal.PrintFatalError(cmd.Flags(), err)
		}
		internal.PrintOutput(cmd.Flags(), p, profileText(p))
	},
}

var profileFetchCmd = &cobra.Command{
	Use:   "fetch <pk>",
	Short: "Fetch another visor's skychat profile over the network",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		var pk cipher.PubKey
		if err := pk.Set(args[0]); err != nil {
			internal.PrintFatalError(cmd.Flags(), err)
		}
		rpcClient, err := clirpc.Client(cmd.Flags())
		if err != nil {
			internal.PrintFatalError(cmd.Flags(), err)
		}
		p, err := rpcClient.ProfileFetch(pk)
		if err != nil {
			internal.PrintFatalError(cmd.Flags(), err)
		}
		internal.PrintOutput(cmd.Flags(), p, profileText(p))
	},
}

func profileText(p visorapi.Profile) string {
	pic := "none"
	if p.Avatar != "" {
		pic = fmt.Sprintf("%d bytes as a data URI", len(p.Avatar))
	}
	return fmt.Sprintf("pk:      %s\nname:    %s\npicture: %s\n", p.PK.Hex(), p.Name, pic)
}

func profileFlagsSetup() {
	profileCmd.AddCommand(profileShowCmd, profileSetCmd, profileFetchCmd)
	profileSetCmd.Flags().StringVar(&profileName, "name", "", "display name, at most 40 characters")
	profileSetCmd.Flags().StringVar(&profileAvatar, "avatar", "", "image file for the picture")
	profileSetCmd.Flags().BoolVar(&profileClearAvatar, "clear-avatar", false, "remove the picture")
	profileSetCmd.Flags().BoolVar(&profileClear, "clear", false, "remove the whole profile")
}
