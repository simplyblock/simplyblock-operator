package cpinformer

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
)

// SSE event names emitted by the control plane (design doc §3.3).
const (
	eventSnapshot = "snapshot"
	eventCreated  = "created"
	eventUpdated  = "updated"
	eventDeleted  = "deleted"
	eventError    = "error"
)

// sseEvent is one dispatched Server-Sent-Event: its `event:` name (empty for an
// unnamed event) and the concatenation of its `data:` lines.
type sseEvent struct {
	Name string
	Data []byte
}

// decodeSSE parses a text/event-stream from r, calling onEvent for each
// dispatched event and onComment for each comment line (the server's `: ping`
// keepalive, used for liveness). It returns nil at a clean end-of-stream and a
// non-nil error only on a transport read error or when onEvent returns one.
//
// It implements the parsing rules relevant to this contract: `field: value`
// lines (a single leading space after the colon is stripped), multi-line
// `data` joined with "\n", comment lines (leading ":"), and dispatch on a blank
// line. `id:` and `retry:` are accepted and ignored — the contract emits no
// `id:`, and reconnect backoff is handled by the caller. A trailing event not
// terminated by a blank line is discarded, per the SSE specification.
func decodeSSE(r io.Reader, onEvent func(sseEvent) error, onComment func()) error {
	br := bufio.NewReader(r)

	var (
		name string
		data []byte
		have bool // a field has been seen since the last dispatch
	)
	dispatch := func() error {
		if !have {
			return nil
		}
		ev := sseEvent{Name: name, Data: data}
		name, data, have = "", nil, false
		return onEvent(ev)
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			l := strings.TrimRight(line, "\n")
			l = strings.TrimRight(l, "\r")
			switch {
			case l == "":
				if e := dispatch(); e != nil {
					return e
				}
			case strings.HasPrefix(l, ":"):
				onComment()
			default:
				field, value, _ := strings.Cut(l, ":")
				value = strings.TrimPrefix(value, " ")
				have = true
				switch field {
				case "event":
					name = value
				case "data":
					if data == nil {
						data = []byte{}
					} else {
						data = append(data, '\n')
					}
					data = append(data, value...)
				default:
					// id, retry, or unknown field — ignored.
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// openStream issues the watch request for one resource path and returns the
// live response. The caller owns resp.Body and must close it. The request
// carries the stream's lifetime via ctx; cancelling ctx aborts the in-flight read.
func openStream(ctx context.Context, cfg StreamConfig, path string) (*http.Response, error) {
	url := strings.TrimRight(cfg.Endpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("watch", "true")
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	client := cfg.Client
	if client == nil {
		// No client timeout: the stream is long-lived. Liveness is enforced by
		// the caller's read-deadline watchdog, not an overall request timeout.
		client = &http.Client{}
	}
	return client.Do(req)
}
