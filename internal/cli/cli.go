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
	"github.com/klhq/oai-plan-proxy/internal/store"
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
  status   show the stored sign-in
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
	case "status":
		return runStatus(env)
	default:
		fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func openStore(env Env) (*store.Store, error) {
	cfg, err := config.Load(env.Getenv, env.UserConfigDir)
	if err != nil {
		return nil, err
	}
	return store.Open(cfg.DataDir)
}

// runStatus prints the stored sign-in. It never prints a token.
func runStatus(env Env) int {
	st, err := openStore(env)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	c, err := st.Load()
	if errors.Is(err, store.ErrNoCredentials) {
		fmt.Fprintf(env.Stdout, "login:     %s\nrun `oai-plan-proxy login` to sign in\n", auth.StateMissing)
		return 0
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "login:     %s\n", st.State())
	fmt.Fprintf(env.Stdout, "account:   %s\n", c.Email)
	fmt.Fprintf(env.Stdout, "client id: %s\n", c.ClientID)
	fmt.Fprintf(env.Stdout, "expires:   %s\n", c.ExpiresAt.UTC().Format(time.RFC3339))
	return 0
}

func runServe(ctx context.Context, env Env) int {
	cfg, err := config.Load(env.Getenv, env.UserConfigDir)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	st, err := store.Open(cfg.DataDir)
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
		Handler:           server.New(server.Options{Login: st}),
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
