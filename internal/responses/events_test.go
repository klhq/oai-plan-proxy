package responses_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/responses"
)

func readAll(t *testing.T, stream string) []responses.Event {
	t.Helper()
	r := responses.NewEventReader(strings.NewReader(stream))
	var out []responses.Event
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
}

func TestEventReaderSplitsAStreamIntoRawEvents(t *testing.T) {
	stream := "\n" +
		"event: a\r\ndata: {\"type\":\"response.created\"}\r\n\r\n" +
		"data: {\"type\":\ndata: \"response.output_text.delta\"}\n\n" +
		"data: {\"type\":\"response.completed\"}"

	got := readAll(t, stream)

	want := []struct{ raw, data, typ string }{
		{"event: a\r\ndata: {\"type\":\"response.created\"}\r\n\r\n", `{"type":"response.created"}`, "response.created"},
		{"data: {\"type\":\ndata: \"response.output_text.delta\"}\n\n", "{\"type\":\n\"response.output_text.delta\"}", "response.output_text.delta"},
		{"data: {\"type\":\"response.completed\"}", `{"type":"response.completed"}`, "response.completed"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if string(got[i].Raw) != w.raw || string(got[i].Data) != w.data || got[i].Type != w.typ {
			t.Errorf("event %d = raw %q data %q type %q, want raw %q data %q type %q",
				i, got[i].Raw, got[i].Data, got[i].Type, w.raw, w.data, w.typ)
		}
	}
}
