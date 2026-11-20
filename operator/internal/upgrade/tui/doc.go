// Package tui is the terminal rendering of an upgrade run: a progress bar for
// the steps a stage holds, a spinner for the rule in flight, and the findings
// and actions printed above them as they happen.
//
// It is one implementation of upgrade.Reporter and holds no decisions. What a
// run does when a check refuses is the runner's business, and this package only
// gets told about it, which is what lets the same run be watched at a terminal
// and logged as a Job in the cluster without either behaving differently.
//
// The live view is deliberately not full screen. A migration is something a
// user reads back afterward, and an alternate screen buffer takes the whole
// record away when the program exits, so completed lines are printed into the
// terminal's own scrollback through tea.Printf and only the bar and the spinner
// are redrawn.
package tui
