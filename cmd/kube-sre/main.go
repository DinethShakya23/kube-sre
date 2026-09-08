package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/DinethShakya23/kube-sre/internal/app"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/logging"
	"github.com/DinethShakya23/kube-sre/internal/schema"
)

const usage = `kube-sre - AI powered Kubernetes operations

usage:
  kube-sre serve [--host H] [--port P]   start the API server
  kube-sre db-init                       create or update the database schema
  kube-sre chat [-q MSG] [flags]         talk to a running server
  kube-sre status [flags]                show server health
  kube-sre replay ID [flags]             replay a recorded episode
  kube-sre digest [--hours N] [flags]    what happened while you were away
  kube-sre postmortem ID [flags]         grounded postmortem of an episode
  kube-sre detector list|new|promote|demote|shadow   manage authored detectors
  kube-sre version                       print the version

client flags: --server URL (default $KUBESRE_URL or http://localhost:8000),
              --key KEY (default $KUBESRE_API_KEY), --user NAME

config file: ~/.kube-sre/.env (or ./.env). Environment variables win over both.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-version":
		fmt.Println("kube-sre", app.Version)
		return 0
	case "serve":
		return serve(args[1:])
	case "db-init":
		return dbInit()
	case "chat":
		return chat(args[1:])
	case "status":
		return status(args[1:])
	case "replay":
		return replay(args[1:])
	case "digest":
		return digestCmd(args[1:])
	case "postmortem":
		return postmortemCmd(args[1:])
	case "detector":
		return detectorCmd(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
	return 2
}

func load() *config.Config {
	cfg := config.FromEnvironment()
	logging.Setup(cfg.LogLevel, cfg.LogFormat)
	return cfg
}

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	host := fs.String("host", "0.0.0.0", "bind host")
	port := fs.Int("port", 8000, "bind port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nStartup failed: %v\n", err)
		return 1
	}
	fmt.Printf("\n  Starting kube-sre on http://%s:%d\n  Press Ctrl+C to stop.\n\n", *host, *port)
	if err := a.Serve(ctx, *host+":"+strconv.Itoa(*port)); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		return 1
	}
	return 0
}

func dbInit() int {
	cfg := load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		return 1
	}
	db, err := app.Migrate(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n  Database error: %v\n\n  %s\n", err, dbHint(err, cfg))
		return 1
	}
	defer db.Close()
	fmt.Printf("  Database schema initialized (%s, v%d).\n  Next: kube-sre serve\n", db.Dialect, schema.Latest())
	return 0
}
