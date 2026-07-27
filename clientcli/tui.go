package clientcli

import (
	"github.com/urfave/cli/v2"

	"github.com/jobs-build/jobs-iroh/tui"
)

// tuiCmd launches the interactive admin TUI over jobs-admin/1.0: live build
// requests (with snapshot watch, node logs, cancel/delete), runner fleet,
// server stats and the ref browser.
func tuiCmd() *cli.Command {
	return &cli.Command{
		Name:  "tui",
		Usage: "interactive admin TUI: builds, runner fleet, server stats, refs",
		Flags: serverFlags(),
		Action: func(c *cli.Context) error {
			ctx, stop := signalCtx(c.Context)
			defer stop()
			return tui.Run(ctx, tui.Config{
				Server: c.String("server"),
				Addrs:  c.StringSlice("addr"),
			})
		},
	}
}
