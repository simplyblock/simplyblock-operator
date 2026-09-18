package layers

import (
	"fmt"
	"log/slog"
)

// Logger is used to emit non-fatal warnings: a best-effort cleanup step that
// failed without being fatal to the verb it ran inside. When nil,
// slog.Default() is resolved at log time so it follows the application's
// configured default. This package stays free of any Kubernetes-specific
// logging dependency, so it is usable outside a Kubernetes context. A caller
// that wants its own log format sets Logger once at startup.
//
// Mirrors atlas-lib/lvm/vdo's own Logger, which this package's lvmVolumeGroup
// and lvmLogicalVolume layers absorb the lifecycle of.
var Logger *slog.Logger

func warnf(format string, args ...any) {
	l := Logger
	if l == nil {
		l = slog.Default()
	}
	l.Warn(fmt.Sprintf(format, args...))
}
