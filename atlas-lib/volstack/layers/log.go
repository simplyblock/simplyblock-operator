// Non-fatal warnings a layer emits along a best-effort path: a hygiene step
// that failed without the layer's own verb failing over it.

package layers

import (
	"fmt"
	"log/slog"
)

// Logger is where a layer's non-fatal warnings go. When nil, slog.Default()
// is resolved at log time so it follows the application's configured
// default. This package stays free of any Kubernetes-specific logging
// dependency, matching atlas-lib/lvm/vdo's own Logger. A caller that wants
// its own log format sets Logger once at startup.
var Logger *slog.Logger

func warnf(format string, args ...any) {
	l := Logger
	if l == nil {
		l = slog.Default()
	}
	l.Warn(fmt.Sprintf(format, args...))
}
