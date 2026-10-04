// The flags the generator script passes are the flags node_configure.py has.
//
// The script is prose until a node runs it: nothing in this repository imports
// it, so a flag that was renamed in sbcli shows up as an init container exiting
// 2 -- argparse refusing an argument -- with the name it refused kept in a log
// the cluster is destroyed with. These cases hold the spellings against what
// simplyblock_web/node_configure.py actually declares.

package utils

import (
	"strings"
	"testing"
)

// --force-format does not exist. The flag is --force, which formats the
// selected devices and clears their partitions, and emitting the longer
// spelling made every block deployment that asked to format its drives fail
// before a node was added.
func TestTheForceFlagIsSpeltAsNodeConfigureDeclaresIt(t *testing.T) {
	_, generator := scripts(t)

	if strings.Contains(generator, "--force-format") {
		t.Error("the generator passes --force-format, which node_configure.py does not accept")
	}
	if !strings.Contains(generator, "--force") {
		t.Error("the generator passes no force flag, so an approved drive format never happens")
	}
}

// Every flag the generator can pass, against the set node_configure.py
// declares. A flag added here that sbcli does not have fails the same way
// --force-format did, and this is the cheapest place to notice.
func TestEveryFlagTheGeneratorPassesIsOneNodeConfigureAccepts(t *testing.T) {
	_, generator := scripts(t)

	// simplyblock_web/node_configure.py, as of sbcli's storage-node image.
	accepted := map[string]bool{
		"--blk-names": true, "--blk-names-exclude": true, "--blk-serials": true,
		"--device-model": true, "--force": true, "--jm-percent": true, "--lblk": true,
		"--max-lvol": true, "--max-size": true, "--model": true,
		"--nodes-per-socket": true, "--nvme-devices": true, "--pci-allowed": true,
		"--pci-blocked": true, "--size-range": true, "--sockets-to-use": true,
		"--upgrade": true,
	}

	for _, field := range strings.Fields(generator) {
		// A flag appears inside the shell assignment that builds ARGS, so it
		// arrives wrapped in whatever quoting that line used.
		flag, _, _ := strings.Cut(strings.Trim(field, `"\`), "=")
		if !strings.HasPrefix(flag, "--") {
			continue
		}
		if !accepted[flag] {
			t.Errorf("the generator passes %s, which node_configure.py does not declare", flag)
		}
	}
}
