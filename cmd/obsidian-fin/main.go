package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"obsidian-fin/internal/cli"
	"obsidian-fin/internal/errs"
)

var version = "0.1.0-dev"

func main() {
	// New SQLite journals/sidecars also inherit private permissions.
	syscall.Umask(0077)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cli.Execute(ctx, cli.New(version, cli.Dependencies{}))
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	os.Exit(errs.ExitCode(err))
}
