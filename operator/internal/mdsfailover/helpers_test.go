// Small helpers the takeover tests share: a background context, a sentinel
// error, and readers of a gathered Prometheus registry.

package mdsfailover

import (
	"context"
	"errors"
)

type contextT = context.Context

var (
	ctxBackground = context.Background()
	errTest       = errors.New("injected failure")
)
