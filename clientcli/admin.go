package clientcli

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/fables-for-robots/jobs-iroh/api"
)

// serverFlags are the connection flags shared by every remote observation
// command.
func serverFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "server", Required: true, EnvVars: []string{"JOBS_SERVER"}, Usage: "endpoint ID of the jobs-server"},
		&cli.StringSliceFlag{Name: "addr", EnvVars: []string{"JOBS_SERVER_ADDR"}, Usage: "direct server address host:port (repeatable; skips discovery and relays)"},
	}
}

// --- watch ---

func watchCmd() *cli.Command {
	return &cli.Command{
		Name:  "watch",
		Usage: "stream one build request's progress until it finishes",
		Flags: append(serverFlags(),
			&cli.StringFlag{Name: "request-id", Required: true, Usage: "request ID printed at submit time"},
			&cli.BoolFlag{Name: "no-logs", Usage: "do not stream the output of running build steps"},
		),
		Action: func(c *cli.Context) error {
			ctx, stop := signalCtx(c.Context)
			defer stop()
			bc, err := dialAPI(ctx, c.String("server"), c.StringSlice("addr"), alpnBuild)
			if err != nil {
				return err
			}
			defer bc.Close()
			lv := cliLiveView(c) // one shared view: tracker Printlns and the
			// watch block must go through the same cursor arithmetic
			var tracker *logTracker
			if !c.Bool("no-logs") {
				tracker = newLogTracker(ctx, bc, lv)
				defer tracker.close()
			}
			final, err := streamWatch(ctx, bc, c.String("request-id"), lv, tracker)
			if err != nil {
				return err
			}
			if final.Phase == "failed" {
				// Same failure UX as remote-build: the failing nodes'
				// stored log tails, then the one-line summary.
				printFailureLogs(ctx, bc, final, errWriter(c), tracker.streamedNodes())
				fmt.Fprintf(errWriter(c), "full failure report (all attempts, durable): jobs-client diagnose --server %s --request %s\n",
					c.String("server"), c.String("request-id"))
				if s := failureSummary(final); s != "" {
					return cli.Exit("request failed: "+s, 1)
				}
			}
			if final.Phase != "done" {
				return cli.Exit("request "+final.Phase, 1)
			}
			return nil
		},
	}
}

// --- admin ---

// adminCmd is the thin frame-call admin surface: it doubles as the admin
// tooling until the TUI milestone.
func adminCmd() *cli.Command {
	return &cli.Command{
		Name:  "admin",
		Usage: "observe a jobs-server: stats, runner fleet, live requests, refs",
		Subcommands: []*cli.Command{
			adminStatsCmd(),
			adminFleetCmd(),
			adminRequestsCmd(),
			adminRefsCmd(),
		},
	}
}

// adminCall dials the admin ALPN and performs one request/reply exchange.
func adminCall(c *cli.Context, t string, body any, wantType string, reply any) error {
	ctx, stop := signalCtx(c.Context)
	defer stop()
	ac, err := dialAPI(ctx, c.String("server"), c.StringSlice("addr"), alpnAdmin)
	if err != nil {
		return err
	}
	defer ac.Close()
	return ac.call(ctx, t, body, wantType, reply)
}

func adminStatsCmd() *cli.Command {
	return &cli.Command{
		Name:  "stats",
		Usage: "server store/scheduler statistics",
		Flags: serverFlags(),
		Action: func(c *cli.Context) error {
			var st api.StatsReply
			if err := adminCall(c, api.TStats, nil, api.TStatsReply, &st); err != nil {
				return err
			}
			w := c.App.Writer
			fmt.Fprintf(w, "store bytes:   %d\n", st.StoreBytes)
			fmt.Fprintf(w, "refs:          %d\n", st.RefCount)
			fmt.Fprintf(w, "requests:      %d\n", st.Requests)
			fmt.Fprintf(w, "nodes tracked: %d\n", st.NodesTracked)
			fmt.Fprintf(w, "uptime:        %s\n", time.Duration(st.UptimeNs).Round(time.Second))
			return nil
		},
	}
}

func adminFleetCmd() *cli.Command {
	return &cli.Command{
		Name:  "fleet",
		Usage: "connected runner fleet",
		Flags: serverFlags(),
		Action: func(c *cli.Context) error {
			var fl api.FleetReply
			if err := adminCall(c, api.TFleet, nil, api.TFleetReply, &fl); err != nil {
				return err
			}
			w := c.App.Writer
			if len(fl.Runners) == 0 {
				fmt.Fprintln(w, "no runners")
				return nil
			}
			for _, r := range fl.Runners {
				fmt.Fprintf(w, "%s  %-16s %-12s %-8s inflight=%d freeCPU=%dm freeMem=%d seen=%s ago\n",
					r.ID, r.Name, r.Platform, r.Size, r.InFlight, r.FreeCPU, r.FreeMem,
					time.Since(time.Unix(0, r.SeenNs)).Round(time.Second))
			}
			return nil
		},
	}
}

func adminRequestsCmd() *cli.Command {
	return &cli.Command{
		Name:  "requests",
		Usage: "live build requests, newest first",
		Flags: serverFlags(),
		Action: func(c *cli.Context) error {
			var rr api.RequestsReply
			if err := adminCall(c, api.TRequests, nil, api.TRequestsReply, &rr); err != nil {
				return err
			}
			w := c.App.Writer
			if len(rr.Requests) == 0 {
				fmt.Fprintln(w, "no requests")
				return nil
			}
			for _, r := range rr.Requests {
				fmt.Fprintf(w, "%s  %-9s %-12s done %d/%d failed %d  k=%s  age=%s\n",
					r.RequestID, r.Phase, r.Platform,
					r.Counts.Done, r.Counts.Total, r.Counts.Failed,
					shortHex(r.K),
					time.Since(time.Unix(0, r.CreatedNs)).Round(time.Second))
			}
			return nil
		},
	}
}

func adminRefsCmd() *cli.Command {
	return &cli.Command{
		Name:  "refs",
		Usage: "browse server refs by prefix",
		Flags: append(serverFlags(),
			&cli.StringFlag{Name: "prefix", Usage: "only refs whose name starts with this prefix"},
			&cli.IntFlag{Name: "limit", Usage: "cap the listing (0 = all)"},
		),
		Action: func(c *cli.Context) error {
			var reply api.RefsReply
			req := api.RefsRequest{Prefix: c.String("prefix"), Limit: c.Int("limit")}
			if err := adminCall(c, api.TRefs, req, api.TRefsReply, &reply); err != nil {
				return err
			}
			w := c.App.Writer
			if len(reply.Refs) == 0 {
				fmt.Fprintln(w, "no refs")
				return nil
			}
			for _, r := range reply.Refs {
				fmt.Fprintf(w, "%s\t%s\n", r.Name, hex.EncodeToString(r.Key))
			}
			return nil
		},
	}
}

// shortHex renders a key's first 8 hex chars for table output.
func shortHex(b []byte) string {
	s := hex.EncodeToString(b)
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
