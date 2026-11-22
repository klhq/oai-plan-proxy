package cli_test

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/store"
)

func TestLoginCompletesFromACallbackURLPastedOnStandardInput(t *testing.T) {
	h := newHarness(t)
	urls := h.fakeAuth(t)
	in, paste := io.Pipe()
	t.Cleanup(func() { paste.Close() })
	h.stdin = in

	done := runAsync(context.Background(), []string{"login", "--port", freePort(t)}, h.env())
	authURL := nextURL(t, urls, done)
	q := mustQuery(t, authURL)
	callback := q.Get("redirect_uri") + "?" + url.Values{
		"code": {"code-1"}, "state": {q.Get("state")}, "client_id": {"oaiapp_issued"},
	}.Encode()
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(paste, callback+"\n")
		written <- err
	}()
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("login never read standard input")
	}
	code := waitExit(t, done)

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, h.stderr.String())
	}
	st, _ := store.Open(h.vars["OAI_PLAN_PROXY_DATA_DIR"])
	if creds, err := st.Load(); err != nil || creds.RefreshToken != "refresh-1" {
		t.Fatalf("stored credentials = %+v, %v; want the pasted sign-in", creds, err)
	}
}

func TestLoginTellsTheOperatorToPasteTheAddressFromAnotherMachine(t *testing.T) {
	h := newHarness(t)
	urls := h.fakeAuth(t)
	h.stdin = strings.NewReader("")
	port := freePort(t)

	done := runAsync(context.Background(), []string{"login", "--port", port}, h.env())
	visitCallback(t, nextURL(t, urls, done), url.Values{"client_id": {"oaiapp_issued"}})
	waitExit(t, done)

	out := h.stdout.String()
	for _, want := range []string{"another machine", "address bar", "paste", "ssh -L " + port + ":127.0.0.1:" + port} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
}
