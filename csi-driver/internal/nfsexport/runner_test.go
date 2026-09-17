// Where exportfs runs, which is not in this container.
//
// exportfs is the userspace half of the host's nfsd: it writes /var/lib/nfs/etab
// and pokes /proc/fs/nfsd, and rpc.mountd reads the same files. Run inside the
// plugin's own mount namespace it would edit a copy nothing serves from, so the
// export would be written, reported as published, and be invisible to every
// client. It is also simply absent from the image, which is the failure that
// surfaces first and the less interesting of the two.

package nfsexport

import (
	"strings"
	"testing"
)

func TestExportfsRunsInTheHostMountNamespace(t *testing.T) {
	name, args := hostCommand("exportfs", "-ra")

	if name != "nsenter" {
		t.Fatalf("exportfs runs as %q, want it entered into the host namespace", name)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--mount=/proc/1/ns/mnt") {
		t.Errorf("args = %v, want the host's mount namespace", args)
	}
	// The separator matters: without it nsenter reads exportfs's own flags.
	if !strings.Contains(joined, "-- exportfs -ra") {
		t.Errorf("args = %v, want the command after a -- separator", args)
	}
}

// The arguments reach the command unchanged, including ones that look like
// nsenter's own.
func TestHostCommandPassesArgumentsThrough(t *testing.T) {
	_, args := hostCommand("exportfs", "-u", "192.168.10.0/24:/mnt/share")

	joined := strings.Join(args, " ")
	if !strings.HasSuffix(joined, "-- exportfs -u 192.168.10.0/24:/mnt/share") {
		t.Errorf("args = %v, want the command and its arguments last", args)
	}
}
