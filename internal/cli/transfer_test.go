package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/cli"
	"github.com/klhq/oai-plan-proxy/internal/store"
)

func TestExportedCredentialsInstallOnAnotherHost(t *testing.T) {
	laptop, server := newHarness(t), newHarness(t)
	laptop.saveCredentials(t, signedIn())
	file := filepath.Join(t.TempDir(), "creds.json")

	if code := cli.Run(context.Background(), []string{"export", file}, laptop.env()); code != 0 {
		t.Fatalf("export exit code = %d; stderr: %s", code, laptop.stderr.String())
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("export file mode = %o, want 600", mode)
	}
	if code := cli.Run(context.Background(), []string{"import", file}, server.env()); code != 0 {
		t.Fatalf("import exit code = %d; stderr: %s", code, server.stderr.String())
	}

	got, err := server.store(t).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != signedIn() {
		t.Fatalf("imported %+v, want %+v", got, signedIn())
	}
}

func TestExportOverAWorldReadableFileMakesItPrivate(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())
	file := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(file, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := cli.Run(context.Background(), []string{"export", file}, h.env()); code != 0 {
		t.Fatalf("export exit code = %d; stderr: %s", code, h.stderr.String())
	}

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("export file mode = %o, want 600", mode)
	}
}

// exportFile writes c to a file through export on a separate host.
func exportFile(t *testing.T, c store.Credentials) string {
	t.Helper()
	other := newHarness(t)
	other.saveCredentials(t, c)
	file := filepath.Join(t.TempDir(), "creds.json")
	if code := cli.Run(context.Background(), []string{"export", file}, other.env()); code != 0 {
		t.Fatalf("export exit code = %d; stderr: %s", code, other.stderr.String())
	}
	return file
}

func TestImportRefusesToReplaceExistingCredentialsWithoutForce(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())
	incoming := signedIn()
	incoming.ClientID = "oaiapp_other"
	file := exportFile(t, incoming)

	code := cli.Run(context.Background(), []string{"import", file}, h.env())

	if code == 0 {
		t.Fatal("import exited 0 over existing credentials without --force")
	}
	if got, _ := h.store(t).Load(); got.ClientID != "oaiapp_test" {
		t.Fatalf("stored client ID = %q, want the original oaiapp_test kept", got.ClientID)
	}
	if !strings.Contains(h.stderr.String(), "--force") {
		t.Fatalf("stderr = %q, want a hint about --force", h.stderr.String())
	}
}

func TestImportWithForceReplacesExistingCredentials(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())
	incoming := signedIn()
	incoming.ClientID = "oaiapp_other"
	file := exportFile(t, incoming)

	code := cli.Run(context.Background(), []string{"import", "--force", file}, h.env())

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, h.stderr.String())
	}
	if got, _ := h.store(t).Load(); got.ClientID != "oaiapp_other" {
		t.Fatalf("stored client ID = %q, want the imported oaiapp_other", got.ClientID)
	}
}

func TestImportRejectsAFileWithoutAUsableSignIn(t *testing.T) {
	h := newHarness(t)
	file := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(file, []byte(`{"OPENAI_API_KEY":null,"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code := cli.Run(context.Background(), []string{"import", file}, h.env())

	if code == 0 {
		t.Fatal("import exited 0 for a file with no client ID or refresh token")
	}
	if _, err := h.store(t).Load(); !errors.Is(err, store.ErrNoCredentials) {
		t.Fatalf("Load after a rejected import = %v, want ErrNoCredentials", err)
	}
}

func TestImportDashReadsTheCredentialsFromStandardInput(t *testing.T) {
	h := newHarness(t)
	b, err := os.ReadFile(exportFile(t, signedIn()))
	if err != nil {
		t.Fatal(err)
	}
	env := h.env()
	env.Stdin = bytes.NewReader(b)

	if code := cli.Run(context.Background(), []string{"import", "-"}, env); code != 0 {
		t.Fatalf("import - exit code = %d; stderr: %s", code, h.stderr.String())
	}

	if got, err := h.store(t).Load(); err != nil || got != signedIn() {
		t.Fatalf("stored %+v, %v; want %+v", got, err, signedIn())
	}
}

// An exported sign-in must live on one host only: the refresh token rotates
// on every use, so two holders end up invalidating each other.
func TestExportMovesTheSignInOffThisHostWithoutRevokingIt(t *testing.T) {
	laptop := newHarness(t)
	laptop.saveCredentials(t, signedIn())
	hostID := laptop.hostID(t)
	file := filepath.Join(t.TempDir(), "creds.json")

	code := cli.Run(context.Background(), []string{"export", file}, laptop.env())

	if code != 0 {
		t.Fatalf("export exit code = %d; stderr: %s", code, laptop.stderr.String())
	}
	if _, err := laptop.store(t).Load(); !errors.Is(err, store.ErrNoCredentials) {
		t.Fatalf("laptop Load after export: err = %v, want ErrNoCredentials", err)
	}
	if got := laptop.hostID(t); got != hostID {
		t.Fatalf("host_id after export = %q, want it kept as %q", got, hostID)
	}
}

func TestAFailedExportKeepsTheSignInOnThisHost(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())
	file := filepath.Join(t.TempDir(), "missing-dir", "creds.json")

	code := cli.Run(context.Background(), []string{"export", file}, h.env())

	if code == 0 {
		t.Fatalf("export into a missing directory exited 0; stdout: %s", h.stdout.String())
	}
	if got, err := h.store(t).Load(); err != nil || got != signedIn() {
		t.Fatalf("Load after a failed export = %+v, %v; want the sign-in kept", got, err)
	}
}

// export - | ssh host import - moves a sign-in without a file on either disk.
func TestExportToStandardOutputPipesIntoImport(t *testing.T) {
	laptop, server := newHarness(t), newHarness(t)
	laptop.saveCredentials(t, signedIn())

	if code := cli.Run(context.Background(), []string{"export", "-"}, laptop.env()); code != 0 {
		t.Fatalf("export - exit code = %d; stderr: %s", code, laptop.stderr.String())
	}
	server.stdin = strings.NewReader(laptop.stdout.String())
	if code := cli.Run(context.Background(), []string{"import", "-"}, server.env()); code != 0 {
		t.Fatalf("import - exit code = %d; stderr: %s; export stdout: %q", code, server.stderr.String(), laptop.stdout.String())
	}

	if got, err := server.store(t).Load(); err != nil || got != signedIn() {
		t.Fatalf("server Load = %+v, %v; want the exported sign-in", got, err)
	}
	if _, err := laptop.store(t).Load(); !errors.Is(err, store.ErrNoCredentials) {
		t.Fatalf("laptop Load after export -: err = %v, want ErrNoCredentials", err)
	}
}

// brokenPipe fails every write, like a pipe whose reader has exited.
type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestAnExportThatCannotWriteToStandardOutputKeepsTheSignIn(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())
	env := h.env()
	env.Stdout = brokenPipe{}

	code := cli.Run(context.Background(), []string{"export", "-"}, env)

	if code == 0 {
		t.Fatal("export - into a broken pipe exited 0")
	}
	if got, err := h.store(t).Load(); err != nil || got != signedIn() {
		t.Fatalf("Load after a failed export - = %+v, %v; want the sign-in kept", got, err)
	}
}
