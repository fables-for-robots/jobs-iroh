package clientcli

import (
	"fmt"

	"github.com/urfave/cli/v2"
)

// logsCmd is the standalone per-node log fetch: the stored head/gap/tail on
// stdout (raw bytes, pipeable), and with --follow the live chunk stream until
// interrupted — the log stream itself carries no completion signal, so a
// follow has no natural end (tail -f semantics).
func logsCmd() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "print one build node's captured output; --follow streams new output live",
		Flags: append(serverFlags(),
			&cli.StringFlag{Name: "node", Required: true, Usage: "full node name, e.g. buildrun_<hex> (as printed by diagnose)"},
			&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "keep streaming new output until interrupted"},
		),
		Action: func(c *cli.Context) error {
			ctx, stop := signalCtx(c.Context)
			defer stop()
			bc, err := dialAPI(ctx, c.String("server"), c.StringSlice("addr"), alpnBuild)
			if err != nil {
				return err
			}
			defer bc.Close()
			follow := c.Bool("follow")
			view, next, done, err := bc.openLogs(ctx, c.String("node"), follow)
			if err != nil {
				return err
			}
			defer done()

			w := c.App.Writer
			w.Write(view.Head)
			if view.GapSize > 0 {
				fmt.Fprintf(w, "\n... [%d bytes omitted] ...\n", view.GapSize)
			}
			w.Write(view.Tail)
			if !follow {
				if len(view.Head) == 0 && len(view.Tail) == 0 {
					fmt.Fprintln(errWriter(c), "(no captured output)")
				}
				return nil
			}

			gen := view.Gen
			for {
				chunk, err := next()
				if err != nil {
					if ctx.Err() != nil {
						return nil // interrupted — the follow's only exit
					}
					return fmt.Errorf("log stream: %w", err)
				}
				if chunk.Gen < gen {
					continue // stale attempt
				}
				if chunk.Gen > gen {
					if gen > 0 {
						fmt.Fprintf(errWriter(c), "--- retried: attempt gen %d ---\n", chunk.Gen)
					}
					gen = chunk.Gen
				}
				w.Write(chunk.Data)
			}
		},
	}
}
