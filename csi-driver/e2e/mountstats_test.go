package e2e

import (
	"strings"
	"testing"
)

// sample is /proc/self/mountstats as a pNFS client prints it: one NFS mount
// beside the ordinary ones, with its per-operation section. Trimmed to the rows
// these tests read, and with the surrounding mounts kept, because telling the
// NFS mount from them is most of the parsing.
const sample = `device rootfs mounted on / with fstype rootfs
device proc mounted on /proc with fstype proc
device 10.20.0.7:/exports/pvc-abc mounted on /var/lib/kubelet/pods/1/volumes/x with fstype nfs4 statvers=1.1
	opts: rw,vers=4.1,rsize=1048576,wsize=1048576
	age: 41
	impl_id: name='',domain='',date='0,0'
	caps: caps=0xfbffdf,wtmult=512,dtsize=32768,bsize=0,namlen=255
	nfsv4: bm0=0xfdffbfff,bm1=0x40f9be3e,bm2=0x803
	sec: flavor=1,pseudoflavor=1
	events: 12 0 0 0 4 0 0 0 0 0 0 0
	bytes: 0 67108864 0 0 0 67108864 0 16384
	RPC iostats version: 1.1  p/v: 100003/4 (nfs)
	xprt:	tcp 0 0 2 0 1 58 58 0 60 0 2 0 0
	per-op statistics
	        NULL: 1 1 0 44 24 0 0 0
	        READ: 3 3 0 528 1024 0 1 2
	       WRITE: 0 0 0 0 0 0 0 0
	      COMMIT: 1 1 0 200 160 0 0 1
	   LAYOUTGET: 2 2 0 464 336 0 0 3
	LAYOUTCOMMIT: 1 1 0 248 168 0 0 1
	LAYOUTRETURN: 0 0 0 0 0 0 0 0

device tmpfs mounted on /run with fstype tmpfs
`

func TestParseNFSOpCountsReadsTheCountersOfTheNamedMount(t *testing.T) {
	counts, err := parseNFSOpCounts(sample, "/var/lib/kubelet/pods/1/volumes/x")
	if err != nil {
		t.Fatalf("parseNFSOpCounts: %v", err)
	}

	for op, want := range map[string]int64{
		"WRITE": 0, "READ": 3, "LAYOUTGET": 2, "LAYOUTCOMMIT": 1,
	} {
		if counts[op] != want {
			t.Errorf("%s = %d, want %d", op, counts[op], want)
		}
	}
}

func TestParseNFSOpCountsFallsBackToTheOnlyNFSMount(t *testing.T) {
	// A pod sees its volume as a bind of the staging mount, so the path a test
	// knows is often not the one the kernel prints.
	counts, err := parseNFSOpCounts(sample, "/spdkvol")
	if err != nil {
		t.Fatalf("parseNFSOpCounts: %v", err)
	}
	if counts["LAYOUTGET"] != 2 {
		t.Errorf("LAYOUTGET = %d, want 2", counts["LAYOUTGET"])
	}
}

func TestParseNFSOpCountsRefusesToGuessBetweenTwoMounts(t *testing.T) {
	second := strings.Replace(sample,
		"device tmpfs mounted on /run with fstype tmpfs",
		"device 10.20.0.8:/other mounted on /elsewhere with fstype nfs4 statvers=1.1\n"+
			"\tper-op statistics\n\t       WRITE: 99 99 0 0 0 0 0 0",
		1)

	// Attributing another mount's writes to this one would turn the whole
	// assertion into noise.
	if _, err := parseNFSOpCounts(second, "/spdkvol"); err == nil {
		t.Fatal("parseNFSOpCounts guessed between two NFS mounts")
	}
}

func TestParseNFSOpCountsReportsAFileWithNoNFSMount(t *testing.T) {
	if _, err := parseNFSOpCounts("device tmpfs mounted on /run with fstype tmpfs\n", "/spdkvol"); err == nil {
		t.Fatal("parseNFSOpCounts found counters in a file naming no NFS mount")
	}
}

func TestParseNFSOpCountsIgnoresTheNonNFSMountsAround(t *testing.T) {
	// The rows above and below the NFS block are other filesystems, and a parser
	// that counted them would attribute their lines to the volume.
	blocks := nfsBlocks(sample)
	if len(blocks) != 1 {
		t.Fatalf("found %d NFS mounts, want 1: %v", len(blocks), blocks)
	}
}
