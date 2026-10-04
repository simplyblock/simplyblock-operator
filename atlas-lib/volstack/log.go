// Where this package says something that is neither a return value nor a
// failure: a step it skipped, and why.
//
// It lives here rather than in layers because the runner has the same need and
// layers already imports this package, so one logger serves both and a consumer
// sets one rather than two.

package volstack

import (
	"fmt"
	"log/slog"
)

// Logger is what this package and its layers report through. When nil,
// slog.Default() is resolved at log time, so output follows whatever the
// application configured. Kept free of any Kubernetes logging dependency, which
// is what lets atlas be used outside one. A caller wanting its own format sets
// this once at startup.
var Logger *slog.Logger

func logger() *slog.Logger {
	if Logger == nil {
		return slog.Default()
	}
	return Logger
}

// Infof reports something a reader needs in order to tell two indistinguishable
// outcomes apart, which is mostly work that was skipped rather than done.
func Infof(format string, args ...any) { logger().Info(fmt.Sprintf(format, args...)) }

// Warnf reports a best-effort step that failed without failing the verb it ran
// inside.
func Warnf(format string, args ...any) { logger().Warn(fmt.Sprintf(format, args...)) }
