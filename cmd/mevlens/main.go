// Command mevlens is the MEVLens observatory: it ingests Arbitrum blocks,
// detects atomic arbitrage and stores the results in ClickHouse.
//
// Usage:
//
//	mevlens follow   -config configs/arbitrum-one.toml [-from N] [-listen ADDR]
//	mevlens backfill -config configs/arbitrum-one.toml -from N -to M
//	mevlens inspect  -config configs/arbitrum-one.toml -block N [-dump DIR]
//	mevlens migrate  -config configs/arbitrum-one.toml
//	mevlens version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "mevlens:", err)
		os.Exit(1)
	}
}

const usage = `mevlens — Arbitrum arbitrage observatory

Commands:
  follow    ingest new blocks continuously (resumes from the last checkpoint)
  backfill  ingest a historical block range (resumable)
  inspect   classify one block and print the result; no database needed
  migrate   create or upgrade the ClickHouse schema
  version   print build information

Run 'mevlens <command> -h' for flags.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("missing command")
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "configs/arbitrum-one.toml", "path to the TOML configuration")

	switch cmd {
	case "follow":
		from := fs.Uint64("from", 0, "start block when there is no checkpoint (default: chain head)")
		listen := fs.String("listen", "", "admin server address (overrides telemetry.listen)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return follow(ctx, *configPath, *from, *listen, stderr)
	case "backfill":
		from := fs.Uint64("from", 0, "first block (inclusive)")
		to := fs.Uint64("to", 0, "last block (inclusive)")
		listen := fs.String("listen", "", "admin server address (overrides telemetry.listen)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *from == 0 || *to < *from {
			return errors.New("backfill: -from and -to are required and -to must be >= -from")
		}
		return backfill(ctx, *configPath, *from, *to, *listen, stderr)
	case "inspect":
		block := fs.Uint64("block", 0, "block number (default: latest)")
		dump := fs.String("dump", "", "directory to write a golden-test fixture to")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return inspect(ctx, *configPath, *block, *dump, stdout, stderr)
	case "migrate":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return migrate(ctx, *configPath, stdout)
	case "version":
		fmt.Fprintln(stdout, version())
		return nil
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "mevlens (unknown build)"
	}
	rev, modified, when := "unknown", "", ""
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				modified = "+dirty"
			}
		case "vcs.time":
			when = " " + s.Value
		}
	}
	return fmt.Sprintf("mevlens %s%s%s (%s)", rev, modified, when, bi.GoVersion)
}
