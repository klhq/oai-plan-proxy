// Command oai-plan-proxy serves an OpenAI-compatible API backed by a ChatGPT
// plan sign-in.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/klhq/oai-plan-proxy/internal/cli"
)

// version and commit are set by release builds:
// go build -ldflags "-X main.version=v1.2.3 -X main.commit=abc1234"
var version, commit string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], cli.Env{
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		Getenv:        os.Getenv,
		UserConfigDir: os.UserConfigDir,
		Version:       version,
		Commit:        commit,
	})
	stop()
	os.Exit(code)
}
