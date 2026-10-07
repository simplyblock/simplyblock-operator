// Installing iptables rules so that a second run adds nothing.
//
// The runner's container can restart inside a pod that keeps its network
// namespace, and with it every rule the previous run appended. Each rule is
// therefore checked (-C) before it is appended (-A), and only iptables' "no
// such rule" answer leads to the append: any other failure is an error, since
// appending blindly after a broken check is how rules end up duplicated.

package netsetup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// checkMissing is iptables -C's exit code for a rule that is not installed.
// iptables also exits 1 for other failures, but those are not a valid rule's
// answer: -C on a well-formed rule in an existing chain fails only this way.
const checkMissing = 1

// Runner runs iptables with args and returns its exit code. An error means
// iptables could not be run at all, not that it exited non-zero.
type Runner func(ctx context.Context, args ...string) (exitCode int, output string, err error)

// ExecIPTables runs the iptables binary on PATH. The image's iptables picks its
// own backend (nft or legacy), which has to match the one kube-proxy and the
// CNI use on the node only for rules they share, and the pod's rules are its
// own.
func ExecIPTables(ctx context.Context, args ...string) (int, string, error) {
	out, err := exec.CommandContext(ctx, "iptables", args...).CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(out), nil
	}
	if err != nil {
		return 0, string(out), err
	}
	return 0, string(out), nil
}

// EnsureRules installs every rule that is not installed yet.
func EnsureRules(ctx context.Context, run Runner, rules []Rule) error {
	for _, rule := range rules {
		code, out, err := run(ctx, ruleArgs("-C", rule)...)
		if err != nil {
			return fmt.Errorf("checking %s: %w", describe(rule), err)
		}
		switch code {
		case 0:
			continue
		case checkMissing:
		default:
			return fmt.Errorf("checking %s: iptables exited %d: %s", describe(rule), code, strings.TrimSpace(out))
		}

		code, out, err = run(ctx, ruleArgs("-A", rule)...)
		if err != nil {
			return fmt.Errorf("appending %s: %w", describe(rule), err)
		}
		if code != 0 {
			return fmt.Errorf("appending %s: iptables exited %d: %s", describe(rule), code, strings.TrimSpace(out))
		}
	}
	return nil
}

// ruleArgs builds the arguments for command on rule. -w waits for the xtables
// lock rather than failing when another process holds it.
func ruleArgs(command string, rule Rule) []string {
	return append([]string{"-w", "-t", rule.Table, command, rule.Chain}, rule.Spec...)
}

func describe(rule Rule) string {
	return rule.Table + "/" + rule.Chain + " " + strings.Join(rule.Spec, " ")
}
