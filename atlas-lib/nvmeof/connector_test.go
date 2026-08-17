package nvmeof

import (
	"context"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/nvme"
)

const (
	// The two UUID-based host NQN forms a connect can name: the one the
	// simplyblock control plane authorizes, and the one the NVMe spec defines.
	sbHostNQN   = "nqn.2014-08.io.simplyblock:uuid:416db8c3-1f3a-4b2e-9a77-1b0d5e6c8f21"
	specHostNQN = "nqn.2014-08.org.nvmexpress:uuid:9f2c1d44-77aa-4e10-8f3b-2c5d6e7a9b01"

	sbHostID   = "416db8c3-1f3a-4b2e-9a77-1b0d5e6c8f21"
	specHostID = "9f2c1d44-77aa-4e10-8f3b-2c5d6e7a9b01"
)

// The kernel keeps hostid and hostnqn strictly 1:1, so the two are resolved as
// a pair. The case that matters is the one in the middle: a target naming its
// own host NQN — every access-controlled volume — must not be handed the node's
// file hostid, which any plain volume on that node has already bound to the
// node's default host NQN.
func TestHostIdentity(t *testing.T) {
	const (
		fileNQN = "nqn.2014-08.org.nvmexpress:uuid:00000000-0000-4000-8000-000000000000"
		fileID  = "00000000-0000-4000-8000-000000000000"
	)
	for _, tc := range []struct {
		name         string
		target       Target
		wantNQN      string
		wantID       string
		wantIDReason string
	}{{
		name:         "no target identity falls back to the node's, as a pair",
		target:       Target{},
		wantNQN:      fileNQN,
		wantID:       fileID,
		wantIDReason: "the node's own file-seeded pair",
	}, {
		name:         "an explicit host NQN derives its id from itself",
		target:       Target{HostNQN: sbHostNQN},
		wantNQN:      sbHostNQN,
		wantID:       sbHostID,
		wantIDReason: "derived from the NQN's own UUID, never the node's file id",
	}, {
		name:         "a spec-form host NQN derives too",
		target:       Target{HostNQN: specHostNQN},
		wantNQN:      specHostNQN,
		wantID:       specHostID,
		wantIDReason: "derived from the NQN's own UUID",
	}, {
		name:         "an explicitly named id wins over the derived one",
		target:       Target{HostNQN: sbHostNQN, HostID: "caller-id"},
		wantNQN:      sbHostNQN,
		wantID:       "caller-id",
		wantIDReason: "the caller's explicit choice",
	}, {
		name:         "a host NQN carrying no UUID is served by an explicit id",
		target:       Target{HostNQN: "nqn.2014-08.org.nvmexpress:client-42", HostID: "caller-id"},
		wantNQN:      "nqn.2014-08.org.nvmexpress:client-42",
		wantID:       "caller-id",
		wantIDReason: "the caller's explicit choice, the only source left",
	}, {
		name:         "an id without an NQN keeps the node's NQN",
		target:       Target{HostID: "caller-id"},
		wantNQN:      fileNQN,
		wantID:       "caller-id",
		wantIDReason: "the caller's explicit choice",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			hostNQN, hostID, err := hostIdentity(tc.target, fileNQN, fileID)
			if err != nil {
				t.Fatalf("hostIdentity: %v", err)
			}
			if hostNQN != tc.wantNQN {
				t.Errorf("hostnqn = %q, want %q", hostNQN, tc.wantNQN)
			}
			if hostID != tc.wantID {
				t.Errorf("hostid = %q, want %q (%s)", hostID, tc.wantID, tc.wantIDReason)
			}
		})
	}
}

// A host NQN with no UUID in it and no explicit id beside it has to fail the
// connect rather than send the NQN with the hostid left off. Omitting it is not
// "no hostid": nvme-cli reads /etc/nvme/hostid and the kernel supplies its own
// default, which is the mismatched pairing — so a quiet connect here is a
// connect under the wrong identity, and it breaks later on some other volume.
func TestHostIdentity_NoDerivableID(t *testing.T) {
	const hostNQN = "nqn.2014-08.org.nvmexpress:client-42"
	_, _, err := hostIdentity(Target{HostNQN: hostNQN}, "node-nqn", "node-id")
	if err == nil {
		t.Fatal("hostIdentity = nil error, want a refusal to pair this NQN with the node's id")
	}
	if !strings.Contains(err.Error(), hostNQN) {
		t.Errorf("err = %v, want it to name the host NQN it could not derive from", err)
	}
}

// The same refusal has to reach a caller through both mechanisms, not just one.
func TestConnectRefusesUnderivableHostIdentity(t *testing.T) {
	tgt := Target{NQN: "nqn.x", Address: "10.0.0.1", HostNQN: "nqn.2014-08.org.nvmexpress:client-42"}

	if _, err := fabricsOptions(tgt, "node-nqn", "node-id"); err == nil {
		t.Error("fabricsOptions = nil error, want the host identity refused")
	}

	c := cliConnector(t, &cliRun{}, fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return notFound()
	}})
	c.hostNQN, c.hostID = "node-nqn", "node-id"
	if _, err := c.connectArgs(tgt); err == nil {
		t.Error("connectArgs = nil error, want the host identity refused")
	}
	// And it must stop the connect, not merely be reported: nvme-cli is never
	// run, so no controller is created under the wrong identity.
	run := &cliRun{}
	c.run = run.run
	if err := c.Connect(context.Background(), tgt); err == nil {
		t.Error("Connect = nil error, want the host identity refused")
	}
	if len(run.calls) != 0 {
		t.Errorf("nvme-cli was run %v, want no connect attempted at all", run.calls)
	}
}
