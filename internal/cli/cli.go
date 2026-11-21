// Package cli dispatches the oai-plan-proxy subcommands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/config"
	"github.com/klhq/oai-plan-proxy/internal/oauth"
	"github.com/klhq/oai-plan-proxy/internal/server"
	"github.com/klhq/oai-plan-proxy/internal/store"
	"github.com/klhq/oai-plan-proxy/internal/tokens"
)

// Env carries the process environment so tests can replace it.
type Env struct {
	Stdout        io.Writer
	Stderr        io.Writer
	Getenv        func(string) string
	UserConfigDir func() (string, error)
	// OAuth overrides the OpenAI endpoints. Tests point it at fakes.
	OAuth oauth.Endpoints
	// HTTPClient is used for OAuth requests. Nil means a default client.
	HTTPClient *http.Client
	// Upstream overrides the API base URL. Tests point it at a fake.
	Upstream string
}

const usage = `usage: oai-plan-proxy <command> [flags]

commands:
  login    sign in with ChatGPT through a browser
  serve    run the HTTP proxy: serve [--strict]
  status   show the stored sign-in
  logout   revoke the sign-in and delete the stored credentials
  export   write the stored credentials to a file: export <file>
  import   install exported credentials: import [--force] <file>
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
		return runServe(ctx, args[1:], env)
	case "status":
		return runStatus(env)
	case "login":
		return runLogin(ctx, args[1:], env)
	case "logout":
		return runLogout(ctx, env)
	case "export":
		return runExport(args[1:], env)
	case "import":
		return runImport(ctx, args[1:], env)
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

// agentName is the agent_name_hint shown on the consent screen.
const agentName = "oai-plan-proxy"

func (env Env) endpoints() oauth.Endpoints {
	if env.OAuth.AuthorizeURL != "" {
		return env.OAuth
	}
	return oauth.OpenAI
}

func (env Env) httpClient() *http.Client {
	if env.HTTPClient != nil {
		return env.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (env Env) upstream() string {
	if env.Upstream != "" {
		return env.Upstream
	}
	return oauth.OpenAI.Resource
}

// runLogin signs in through the browser and stores the result.
func runLogin(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	port := fs.Int("port", 1455, "loopback callback port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := openStore(env)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	hostID, err := st.HostID()
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	// A stored client ID, even from a dead sign-in, makes this a returning
	// sign-in instead of a new registration.
	var clientID string
	if prev, err := st.Load(); err == nil {
		clientID = prev.ClientID
	}

	res, err := oauth.Login(ctx, oauth.LoginOptions{
		Endpoints: env.endpoints(),
		Params:    oauth.Params{ClientID: clientID, HostID: hostID, AgentName: agentName, Port: *port},
		Out:       env.Stdout,
		Client:    env.httpClient(),
	})
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}

	unlock, err := st.Lock(ctx)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	defer unlock()
	if err := st.Save(store.Credentials{
		ClientID:     res.ClientID,
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		IDToken:      res.IDToken,
		ExpiresAt:    res.ExpiresAt,
		Email:        res.Email,
	}); err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "Signed in as %s (client %s).\n", res.Email, res.ClientID)
	return 0
}

// runLogout revokes the refresh token and deletes the stored credentials.
// The host id stays.
func runLogout(ctx context.Context, env Env) int {
	st, err := openStore(env)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	// The lock keeps a running serve from rotating the refresh token while
	// it is being revoked.
	unlock, err := st.Lock(ctx)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	defer unlock()
	c, err := st.Load()
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	// A dead sign-in has no refresh token left to revoke.
	if c.RefreshToken != "" {
		if err := oauth.Revoke(ctx, env.httpClient(), env.endpoints(), c.ClientID, c.RefreshToken); err != nil {
			fmt.Fprintf(env.Stderr, "%v\nthe credentials were kept; run logout again\n", err)
			return 1
		}
	}
	if err := st.Delete(); err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	fmt.Fprintln(env.Stdout, "Signed out.")
	return 0
}

// runExport writes the stored credentials to a file another host can import.
func runExport(args []string, env Env) int {
	if len(args) != 1 {
		fmt.Fprintln(env.Stderr, "usage: oai-plan-proxy export <file>")
		return 2
	}
	st, err := openStore(env)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	c, err := st.Load()
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if err := writePrivateJSON(args[0], c); err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "Exported the sign-in to %s. It holds live tokens: move it over SSH and delete it after import.\n", args[0])
	return 0
}

// writePrivateJSON writes v to path readable by the owner only. Chmod covers
// a file that already existed with wider permissions.
func writePrivateJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create export file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("restrict export file: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("write export file: %w", err)
	}
	return f.Close()
}

// runImport installs credentials exported on another host.
func runImport(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("force", false, "replace credentials that are already stored")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(env.Stderr, "usage: oai-plan-proxy import [--force] <file>")
		return 2
	}
	file := fs.Arg(0)
	b, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	var c store.Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		fmt.Fprintf(env.Stderr, "parse %s: %v\n", file, err)
		return 1
	}
	if c.ClientID == "" || c.RefreshToken == "" {
		fmt.Fprintf(env.Stderr, "%s holds no usable sign-in; create it with `oai-plan-proxy export`\n", file)
		return 1
	}
	st, err := openStore(env)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	unlock, err := st.Lock(ctx)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	defer unlock()
	if _, err := st.Load(); !*force && !errors.Is(err, store.ErrNoCredentials) {
		fmt.Fprintln(env.Stderr, "credentials are already stored; pass --force to replace them")
		return 1
	}
	if err := st.Save(c); err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "Imported the sign-in for %s (client %s).\n", c.Email, c.ClientID)
	return 0
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

func runServe(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	strict := fs.Bool("strict", false, "reject preview-limited parameters with 400 instead of dropping them")
	if err := fs.Parse(args); err != nil {
		return 2
	}
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
	mgr := tokens.New(tokens.Options{Store: st, Client: env.httpClient(), Endpoints: env.endpoints()})
	refreshCtx, stopRefresh := context.WithCancel(ctx)
	defer stopRefresh()
	go mgr.Run(refreshCtx)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(env.Stderr, "listen on %s: %v\n", cfg.Listen, err)
		return 1
	}
	srv := &http.Server{
		Handler: server.New(server.Options{
			Login:    st,
			Tokens:   mgr,
			Upstream: env.upstream(),
			// No overall timeout: response streams run for minutes. The
			// caller's context still cancels a stalled request.
			Client: &http.Client{Transport: env.httpClient().Transport},
			APIKey: cfg.APIKey,
			Strict: *strict,
		}),
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
