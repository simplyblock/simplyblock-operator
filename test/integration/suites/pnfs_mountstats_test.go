// The mountstats reading the pNFS spec's verdict rests on, tested on its own:
// the spec runs only on a cluster booted for it, and a parser that miscounted
// would turn its one assertion into noise.

package suites

import "testing"

// Two NFS mounts, as a node can carry: counts belong to the mount whose
// header precedes them.
const sampleMountstats = `device rootfs mounted on / with fstype rootfs
device 10.5.0.9:/other mounted on /mnt/other with fstype nfs4 statvers=1.1
	opts:	rw,vers=4.1
	per-op statistics
	        NULL: 1 1 0 44 24 0 0 0 0
	       WRITE: 900 900 0 1 2 3 4 5 0
	   LAYOUTGET: 0 0 0 0 0 0 0 0 0
device 10.5.0.2:/var/mnt/sb-pnfs/vol mounted on /mnt/sb-pnfs with fstype nfs4 statvers=1.1
	opts:	rw,vers=4.1
	events:	1 2 3
	per-op statistics
	        NULL: 1 1 0 44 24 0 0 0 0
	        READ: 0 0 0 0 0 0 0 0 0
	       WRITE: 0 0 0 0 0 0 0 0 0
	   LAYOUTGET: 3 3 0 600 400 1 2 3 0
	LAYOUTCOMMIT: 1 1 0 300 200 1 1 2 0
device proc mounted on /proc with fstype proc
`

func TestParseMountOps(t *testing.T) {
	t.Run("counts come from the named mount only", func(t *testing.T) {
		ops, err := parseMountOps(sampleMountstats, "/mnt/sb-pnfs")
		if err != nil {
			t.Fatalf("parseMountOps: %v", err)
		}
		for op, want := range map[string]int{"WRITE": 0, "LAYOUTGET": 3, "LAYOUTCOMMIT": 1, "READ": 0} {
			if ops[op] != want {
				t.Errorf("%s = %d, want %d (the other mount's counts leaked in?)", op, ops[op], want)
			}
		}
	})

	t.Run("a mountpoint that is a prefix of another does not match it", func(t *testing.T) {
		if _, err := parseMountOps(sampleMountstats, "/mnt/sb"); err == nil {
			t.Fatal("parseMountOps matched /mnt/sb against /mnt/sb-pnfs")
		}
	})

	t.Run("a missing mount is an error, not zero counts", func(t *testing.T) {
		if _, err := parseMountOps(sampleMountstats, "/mnt/absent"); err == nil {
			t.Fatal("parseMountOps returned counts for a mount that is not there")
		}
	})
}
