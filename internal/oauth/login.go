package oauth

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LoginOptions configures an interactive sign-in.
type LoginOptions struct {
	Endpoints Endpoints
	// Params.Port is the loopback callback port. Zero picks a free port.
	Params Params
	// Out receives the authorization URL and instructions.
	Out    io.Writer
	Client *http.Client
	// In, when set, is read for a pasted callback URL while Login waits.
	In io.Reader
	// Err receives the reasons a pasted line was rejected. Nil discards them.
	Err io.Writer
}

// Login runs one interactive sign-in. It listens on 127.0.0.1 only, prints
// the authorization URL, and waits for the browser to return to the callback.
func Login(ctx context.Context, opts LoginOptions) (Result, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opts.Params.Port))
	if err != nil {
		return Result{}, fmt.Errorf("listen for the sign-in callback: %w", err)
	}
	defer ln.Close()

	p := opts.Params
	p.Port = ln.Addr().(*net.TCPAddr).Port
	flow, err := NewFlow(opts.Endpoints, p)
	if err != nil {
		return Result{}, err
	}

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		res, err := flow.Exchange(r.Context(), r.URL.Query(), opts.Client)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "Sign-in failed: %v\nReturn to the terminal for details.\n", err)
		} else {
			fmt.Fprintln(w, "Signed in. You can close this tab.")
		}
		select {
		case done <- outcome{res, err}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	// Shutdown, not Close: the callback handler reports its outcome before
	// it returns, and Close would cut the browser's page off mid-response.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), callbackShutdownTimeout)
		defer cancel()
		if srv.Shutdown(shutdownCtx) != nil {
			_ = srv.Close()
		}
	}()

	// finished stops the paste reader once Login returns, so a line pasted
	// after the sign-in completed never spends the code a second time.
	finished := make(chan struct{})
	defer close(finished)
	if opts.In != nil {
		errOut := opts.Err
		if errOut == nil {
			errOut = io.Discard
		}
		go readPasted(opts.In, errOut, finished, func(callback url.Values) {
			// A wrong state is a paste from another sign-in. Nothing was
			// spent, so the operator can paste again.
			if err := flow.checkState(callback); err != nil {
				fmt.Fprintln(errOut, err)
				return
			}
			res, err := flow.Exchange(ctx, callback, opts.Client)
			select {
			case done <- outcome{res, err}:
			default:
			}
		})
	}

	fmt.Fprintf(opts.Out, "Open this URL in a browser to sign in:\n\n%s\n\n", flow.URL)
	fmt.Fprintf(opts.Out, "Waiting for the callback on 127.0.0.1:%d. On a remote host, forward it first:\n", p.Port)
	fmt.Fprintf(opts.Out, "  ssh -L %d:127.0.0.1:%d <this host>\n", p.Port, p.Port)

	select {
	case o := <-done:
		return o.res, o.err
	case <-ctx.Done():
		return Result{}, fmt.Errorf("sign-in cancelled: %w", ctx.Err())
	}
}

// callbackShutdownTimeout bounds how long Login waits for the callback page
// to reach the browser.
const callbackShutdownTimeout = 5 * time.Second

// readPasted reads lines from in until it ends and hands each pasted
// callback URL's query to complete, until finished closes.
func readPasted(in io.Reader, errOut io.Writer, finished <-chan struct{}, complete func(url.Values)) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		select {
		case <-finished:
			// Keep draining, so a writer never blocks, but act on nothing.
			continue
		default:
		}
		u, err := url.Parse(strings.TrimSpace(sc.Text()))
		if err != nil || !u.Query().Has("state") {
			// The line is not echoed: it may hold part of a code.
			fmt.Fprintln(errOut, pasteHint)
			continue
		}
		complete(u.Query())
	}
}

// pasteHint tells the operator what to paste after a rejected line.
const pasteHint = "That is not the callback address. Copy the browser's address bar after approving, then paste the full address here."
