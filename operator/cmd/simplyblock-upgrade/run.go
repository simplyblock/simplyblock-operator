// What every command does before it does its own work: connect, choose a
// reporter, and build the scope the framework's rules run against. It is one
// function so the three commands cannot differ in how they read the cluster,
// which is the difference that would make a preflight prove something about a
// cluster the migration then does not run against.

package main

import (
	"os"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/catalog"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/tui"
)

// session is one command's connection to a cluster: the runner it drives, and
// the reporter it has to give back.
type session struct {
	Runner   *upgrade.Runner
	Reporter upgrade.Reporter
	Client   client.Client
}

// Close gives the terminal back.
func (s *session) Close() error { return s.Reporter.Close() }

// newSession connects, builds the catalog, and returns a runner over the
// cluster.
//
// readOnly wraps the client so that nothing the stage runs can write. It is set
// for the preflight, which promises that nothing changed, and for a dry run of
// the other two.
func newSession(global *globalOptions, stage upgrade.Stage, dryRun bool) (*session, error) {
	if err := mustNamespace(global); err != nil {
		return nil, err
	}

	c, err := newClient(global)
	if err != nil {
		return nil, err
	}
	if dryRun {
		c = upgrade.NewReadOnlyClient(c)
	}

	reporter := newReporter(global)
	scope := upgrade.NewScope(c, global.Namespace, stage, global.options(dryRun), newLogger(global), reporter)

	return &session{
		Runner:   upgrade.NewRunner(catalog.Default(), scope),
		Reporter: reporter,
		Client:   c,
	}, nil
}

// newReporter picks the rendering. A terminal gets the progress view, and
// everything else gets lines, because a progress bar redrawing itself into a
// Job's log is a log nobody can read.
func newReporter(global *globalOptions) upgrade.Reporter {
	if global.Plain || !tui.Interactive(os.Stdout) {
		return &upgrade.TextReporter{Out: os.Stdout, Verbose: global.Verbose}
	}
	return tui.New(os.Stdout)
}

// newLogger is where a rule writes what it is doing, as distinct from what it
// has to say. Diagnosis goes to stderr so it does not interleave with the
// report a user is reading on stdout.
func newLogger(global *globalOptions) logr.Logger {
	level := 0
	if global.Verbose {
		level = 1
	}
	stdr.SetVerbosity(level)
	return stdr.New(newStderrLogger())
}
