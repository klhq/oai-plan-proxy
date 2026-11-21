// Package cli dispatches the oai-plan-proxy subcommands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/config"
	"github.com/klhq/oai-plan-proxy/internal/server"
)

// Env carries the process environment so tests can replace it.
type Env struct {
	Stdout        io.Writer
	Stderr        io.Writer
	Getenv        func(string) string
	UserConfigDir func() (string, error)
}

const usage = `usage: oai-plan-proxy <command> [flags]

commands:
  serve    run the HTTP proxy
`

// shutdownTimeout bounds how long serve waits for in-flight requests.
const shutdownTimeout = 10 * time.Second

// Run executes the command named by args[0] and returns the exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(ctx, env)
	default:
		fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// noLogin reports a missing sign-in until the credential store exists.
type noLogin struct{}

func (noLogin) State() auth.State { return auth.StateMissing }

func runServe(ctx context.Context, env Env) int {
	cfg, err := config.Load(env.Getenv, env.UserConfigDir)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(env.Stderr, "listen on %s: %v\n", cfg.Listen, err)
		return 1
	}
	srv := &http.Server{
		Handler:           server.New(server.Options{Login: noLogin{}}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(env.Stderr, "listening on %s\n", ln.Addr())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		fmt.Fprintf(env.Stderr, "serve: %v\n", err)
		return 1
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(env.Stderr, "shutdown: %v\n", err)
		return 1
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(env.Stderr, "serve: %v\n", err)
		return 1
	}
	return 0
}
