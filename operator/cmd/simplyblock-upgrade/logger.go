// The diagnostic log, which is not the report. A rule writes what it is doing
// here and what it has to say through the reporter, and the two go to different
// streams so that a progress view on stdout is not broken apart by a line about
// a retried read.

package main

import (
	"log"
	"os"
)

// newStderrLogger builds the standard-library logger stdr wraps. The prefix is
// empty and the flags carry the time only: the file and line a message came
// from is noise in a tool whose messages are about a cluster rather than about
// itself.
func newStderrLogger() *log.Logger {
	return log.New(os.Stderr, "", log.Ltime)
}
