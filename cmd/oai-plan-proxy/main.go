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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], cli.Env{
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		Getenv:        os.Getenv,
		UserConfigDir: os.UserConfigDir,
	})
	stop()
	os.Exit(code)
}
