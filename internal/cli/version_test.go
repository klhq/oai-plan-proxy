package cli_test

import (
	"context"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/cli"
)

func TestVersionPrintsTheStampedVersionAndCommit(t *testing.T) {
	h := newHarness(t)
	env := h.env()
	env.Version = "v9.8.7"
	env.Commit = "abc1234"

	code := cli.Run(context.Background(), []string{"version"}, env)

	want := "oai-plan-proxy v9.8.7 (commit abc1234, " + runtime.Version() + ", " + runtime.GOOS + "/" + runtime.GOARCH + ")\n"
	if code != 0 || h.stdout.String() != want {
		t.Fatalf("exit code = %d, stdout = %q; want 0 and %q", code, h.stdout.String(), want)
	}
}

func TestVersionFallsBackToTheGoBuildInfo(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"go install", &debug.BuildInfo{
			Main:     debug.Module{Version: "v1.2.3"},
			Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef0123"}},
		}, "oai-plan-proxy v1.2.3 (commit 0123456789ab, "},
		{"no build info", nil, "oai-plan-proxy (devel) (commit unknown, "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			env := h.env()
			env.BuildInfo = func() (*debug.BuildInfo, bool) { return tc.info, tc.info != nil }

			code := cli.Run(context.Background(), []string{"version"}, env)

			if code != 0 || !strings.HasPrefix(h.stdout.String(), tc.want) {
				t.Fatalf("exit code = %d, stdout = %q; want 0 and a line starting %q", code, h.stdout.String(), tc.want)
			}
		})
	}
}

func TestTheVersionFlagPrintsTheVersion(t *testing.T) {
	h := newHarness(t)
	env := h.env()
	env.Version = "v9.8.7"

	code := cli.Run(context.Background(), []string{"--version"}, env)

	if code != 0 || !strings.HasPrefix(h.stdout.String(), "oai-plan-proxy v9.8.7 ") {
		t.Fatalf("exit code = %d, stdout = %q; want 0 and the version", code, h.stdout.String())
	}
}

func TestServeLogsItsVersionAtStartup(t *testing.T) {
	h := newHarness(t)
	h.vars["OAI_PLAN_PROXY_LOG_FORMAT"] = "json"
	h.vars["OAI_PLAN_PROXY_LISTEN"] = freeAddr(t)
	env := h.env()
	env.Version = "v9.8.7"
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, []string{"serve"}, env)
	waitForGet(t, "http://"+h.vars["OAI_PLAN_PROXY_LISTEN"]+"/healthz").Body.Close()

	stopServe(t, cancel, done)

	recs := logRecords(t, h.stderr.String(), "listening")
	if len(recs) != 1 || recs[0]["version"] != "v9.8.7" {
		t.Fatalf("listening records = %v, want version v9.8.7", recs)
	}
}
