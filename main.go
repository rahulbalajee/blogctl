// Command blogctl drafts posts for rahulbalajee.com through its Blog API, for
// people and AI agents alike. It cannot publish; see docs/agent-workflow.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rahulbalajee/blogctl/internal/cli"
)

// version is stamped by `make build` with -X main.version; a `go install`
// build reports its module version instead (see cli.ResolveVersion).
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The first signal cancels the command; restoring the default handling
	// then lets a second one end the process even if something still waits.
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := cli.Run(ctx, cli.Env{
		Args:    os.Args[1:],
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Version: cli.ResolveVersion(version),
	})
	stop()
	os.Exit(code)
}
