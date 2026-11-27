// The operator pod's log, as the simulator writes it: one line per thing the
// simulated operator does to an operation, in the shape controller-runtime's
// development console encoder gives a reconcile line, with the object's key
// as its context. The console's activity view filters the real operator's
// log on exactly that key, so the mock has to carry it the same way.
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// operatorLogCap bounds the ring; the console asks for the last 2000 lines.
const operatorLogCap = 4000

// OperatorLog is a ring of log lines the simulated operator has written.
type OperatorLog struct {
	mu    sync.Mutex
	lines []string
	now   func() time.Time
	seq   uint64
}

func NewOperatorLog(now func() time.Time) *OperatorLog {
	l := &OperatorLog{now: now}
	for _, m := range []string{
		"setup\tstarting manager",
		"controller-runtime.metrics\tServing metrics server\t{\"bindAddress\": \":8443\", \"secure\": true}",
		"setup\tstarting the simulated reconcilers\t{\"interval\": \"4s\"}",
	} {
		parts := strings.SplitN(m, "\t", 2)
		l.append("INFO", parts[0], parts[1])
	}
	return l
}

// Reconcile writes one reconcile line about an object: the message, then the
// context controller-runtime adds to every line of a reconcile (controller,
// controllerKind, the object's key under its kind, namespace, name, and a
// reconcile id), followed by any extra pairs.
func (l *OperatorLog) Reconcile(level, kind, ns, name, msg string, extra ...string) {
	controller := strings.ToLower(kind)
	ctx := []string{
		fmt.Sprintf("%q: %q", "controller", controller),
		fmt.Sprintf("%q: %q", "controllerKind", kind),
		fmt.Sprintf("%q: {%q: %q, %q: %q}", kind, "name", name, "namespace", ns),
		fmt.Sprintf("%q: %q", "namespace", ns),
		fmt.Sprintf("%q: %q", "name", name),
		fmt.Sprintf("%q: %q", "reconcileID", l.reconcileID()),
	}
	for i := 0; i+1 < len(extra); i += 2 {
		ctx = append(ctx, fmt.Sprintf("%q: %q", extra[i], extra[i+1]))
	}
	l.append(level, controller, msg+"\t{"+strings.Join(ctx, ", ")+"}")
}

func (l *OperatorLog) append(level, logger, rest string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := fmt.Sprintf("%s\t%s\t%s\t%s", l.now().UTC().Format("2006-01-02T15:04:05.000Z"), level, logger, rest)
	l.lines = append(l.lines, line)
	if len(l.lines) > operatorLogCap {
		l.lines = l.lines[len(l.lines)-operatorLogCap:]
	}
}

func (l *OperatorLog) reconcileID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", l.seq*2654435761, l.seq&0xffff, l.seq&0xfff, (l.seq*7)&0xfff, l.seq*1099511628211)
}

// Tail is the last n lines, oldest first; n <= 0 is every line.
func (l *OperatorLog) Tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.lines
	if n > 0 && n < len(out) {
		out = out[len(out)-n:]
	}
	return append([]string(nil), out...)
}
