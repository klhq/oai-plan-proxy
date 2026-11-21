package responses

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// Event is one server-sent event from the upstream stream.
type Event struct {
	// Raw is the event exactly as received, including its blank-line
	// terminator, so it can be forwarded unchanged.
	Raw []byte
	// Data is the event's data lines joined with newlines.
	Data []byte
	// Type is the "type" field of the JSON data.
	Type string
}

// EventReader splits a server-sent event stream into events.
type EventReader struct {
	r *bufio.Reader
}

// NewEventReader reads events from r.
func NewEventReader(r io.Reader) *EventReader {
	return &EventReader{r: bufio.NewReader(r)}
}

// Next returns the next event. It returns io.EOF after the last one.
// bufio.Reader.ReadBytes has no line length limit, unlike bufio.Scanner, so
// a large inline image in an event does not break the stream.
func (e *EventReader) Next() (Event, error) {
	var ev Event
	var data [][]byte
	for {
		line, err := e.r.ReadBytes('\n')
		ev.Raw = append(ev.Raw, line...)
		trimmed := bytes.TrimRight(line, "\r\n")
		if v, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(v, []byte(" ")))
		}
		if err != nil || len(trimmed) == 0 && len(line) > 0 {
			if len(bytes.TrimSpace(ev.Raw)) == 0 {
				if err == nil {
					ev.Raw = ev.Raw[:0]
					continue
				}
				return Event{}, err
			}
			ev.Data = bytes.Join(data, []byte("\n"))
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(ev.Data, &head)
			ev.Type = head.Type
			return ev, nil
		}
	}
}
