package cpinformer

import (
	"strings"
	"testing"
)

func TestDecodeSSE(t *testing.T) {
	// A representative stream: a comment (ping), a named snapshot event, a
	// multi-line data event, an unnamed event, then a trailing unterminated
	// event that must be discarded.
	stream := strings.Join([]string{
		": ping 123",
		"",
		"event: snapshot",
		"retry: 3000",
		`data: [{"id":"a"}]`,
		"",
		"event: updated",
		"data: line1",
		"data: line2",
		"",
		"data: unnamed",
		"",
		"event: discarded", // no blank line terminator, then EOF
	}, "\n")

	var events []sseEvent
	var comments int
	err := decodeSSE(strings.NewReader(stream),
		func(ev sseEvent) error { events = append(events, ev); return nil },
		func() { comments++ },
	)
	if err != nil {
		t.Fatalf("decodeSSE: %v", err)
	}

	if comments != 1 {
		t.Errorf("comments = %d, want 1", comments)
	}
	want := []sseEvent{
		{Name: "snapshot", Data: []byte(`[{"id":"a"}]`)},
		{Name: "updated", Data: []byte("line1\nline2")},
		{Name: "", Data: []byte("unnamed")},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].Name != w.Name || string(events[i].Data) != string(w.Data) {
			t.Errorf("event[%d] = {%q, %q}, want {%q, %q}",
				i, events[i].Name, events[i].Data, w.Name, w.Data)
		}
	}
}

func TestDecodeSSE_LeadingSpaceStripped(t *testing.T) {
	// Exactly one leading space after the colon is stripped; further spaces are
	// data.
	var got sseEvent
	err := decodeSSE(strings.NewReader("data:  two-spaces\n\n"),
		func(ev sseEvent) error { got = ev; return nil }, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != " two-spaces" {
		t.Errorf("data = %q, want %q", got.Data, " two-spaces")
	}
}

func TestDecodeSSE_CRLF(t *testing.T) {
	var got sseEvent
	err := decodeSSE(strings.NewReader("event: created\r\ndata: x\r\n\r\n"),
		func(ev sseEvent) error { got = ev; return nil }, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "created" || string(got.Data) != "x" {
		t.Errorf("got {%q, %q}, want {created, x}", got.Name, got.Data)
	}
}
