// A multi-attach deployment document: its data devices are LUNs the control
// plane creates on the storage array when each node is added, so the floors
// count nodes rather than the stated stripe (the cluster runs 1+0), every node
// names exactly one local device (its journal), and the template's multiAttach
// block reaches the cluster the document creates.

package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func blockGroupsOf(workers []string, count int) []simplyblockv1alpha2.NodeGroup {
	groups := make([]simplyblockv1alpha2.NodeGroup, 0, len(workers))
	for i, worker := range workers {
		group := simplyblockv1alpha2.NodeGroup{
			Name:           fmt.Sprintf("group-%d", i+1),
			Workers:        []string{worker},
			MgmtInterface:  "eth1",
			DataInterfaces: []string{"eth2"},
		}
		if count > 0 {
			devices := make([]string, 0, count)
			for d := 0; d < count; d++ {
				devices = append(devices, fmt.Sprintf("/dev/disk/by-id/nvme-local-%d", d))
			}
			group.Devices = &simplyblockv1alpha2.DeviceSelection{Block: devices}
		}
		groups = append(groups, group)
	}
	return groups
}

func multiAttachTemplate(ftt int32) *simplyblockv1alpha2.MultiAttachSpec {
	return &simplyblockv1alpha2.MultiAttachSpec{
		FaultTolerance: ftt,
		Array: &simplyblockv1alpha2.StorageArraySpec{
			Plugin:   "netapp-ontap",
			Endpoint: "https://ontap.example",
			Protocol: simplyblockv1alpha2.StorageArrayProtocolISCSI,
			LunSize:  resource.MustParse("2Ti"),
			Credentials: simplyblockv1alpha2.StorageArrayCredentials{
				Source:    simplyblockv1alpha2.StorageArrayCredentialsSourceSecret,
				SecretRef: &corev1.LocalObjectReference{Name: "ontap"},
			},
		},
	}
}

func multiAttachFindings(findings []finding) []finding {
	var out []finding
	for _, f := range findings {
		switch f.reason {
		case StripeUnsupported, StripeBelowMinimumNodes, StripeBelowMinimumWorkers,
			StripeSingleNodeCount, StripeBelowMinimumDevices, StripeBelowMinimumFailureDomains,
			MultiAttachJournalDevice:
			out = append(out, f)
		}
	}
	return out
}

func TestAMultiAttachDocumentIsHeldToItsOwnFloors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ftt      int32
		workers  int
		devices  int // named per worker; 0 names none
		slots    int32
		reason   string
		mentions []string
	}{
		// The stated 2+1 stripe would need four nodes; a multi-attach cluster
		// runs 1+0 and needs one node more than the failures it tolerates.
		{name: "FTT 1 on two workers, a journal device each", ftt: 1, workers: 2, devices: 1},
		{name: "FTT 2 on three workers", ftt: 2, workers: 3, devices: 1},
		{name: "FTT 1 on one worker", ftt: 1, workers: 1, devices: 1,
			reason: StripeBelowMinimumNodes, mentions: []string{"2 storage nodes", "fault tolerance 1"}},
		{name: "FTT 2 on two workers", ftt: 2, workers: 2, devices: 1,
			reason: StripeBelowMinimumNodes, mentions: []string{"3 storage nodes"}},
		{name: "two local devices per node", ftt: 1, workers: 2, devices: 2,
			reason: MultiAttachJournalDevice, mentions: []string{"group-1", "exactly one", "2"}},
		{name: "no device named", ftt: 1, workers: 2, devices: 0,
			reason: MultiAttachJournalDevice, mentions: []string{"group-1", "names none"}},
		{name: "two nodes per worker, two devices each", ftt: 1, workers: 2, devices: 2, slots: 2},
		{name: "two nodes per worker on one worker", ftt: 1, workers: 1, devices: 2, slots: 2,
			reason: StripeBelowMinimumWorkers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := workerNames(tc.workers)
			config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
				c.Spec.Approved = false
				c.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
					DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1))}
				c.Spec.Cluster.RaidDevices = ptr.To(true)
				c.Spec.Cluster.MultiAttach = multiAttachTemplate(tc.ftt)
				c.Spec.NodeSets[0].Groups = blockGroupsOf(names, tc.devices)
				if tc.slots > 0 {
					c.Spec.Cluster.NodesPerSocket = ptr.To(tc.slots)
				}
			})
			found := multiAttachFindings(findingsOf(t, config, workers(names...)...))
			if tc.reason == "" {
				if len(found) != 0 {
					t.Fatalf("findings = %+v, want none", found)
				}
				return
			}
			if len(found) != 1 || found[0].reason != tc.reason {
				t.Fatalf("findings = %+v, want one %s", found, tc.reason)
			}
			for _, want := range tc.mentions {
				if !strings.Contains(found[0].message, want) {
					t.Errorf("the finding does not mention %q: %s", want, found[0].message)
				}
			}
		})
	}
}

func TestTheMultiAttachBlockReachesTheCluster(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.RaidDevices = ptr.To(true)
		c.Spec.Cluster.MultiAttach = multiAttachTemplate(1)
		c.Spec.NodeSets[0].Groups = blockGroupsOf([]string{"worker-1", "worker-2"}, 1)
	})
	cluster, err := reconcilerFor(t).buildCluster(config, theCluster)
	if err != nil {
		t.Fatalf("buildCluster: %v", err)
	}
	ma := cluster.Spec.MultiAttach
	if ma == nil || ma.FaultTolerance != 1 || ma.Array == nil || ma.Array.Plugin != "netapp-ontap" ||
		ma.Array.Credentials.SecretRef.Name != "ontap" {
		t.Fatalf("cluster multiAttach = %+v", ma)
	}
	if ma == config.Spec.Cluster.MultiAttach {
		t.Error("the cluster shares the document's multiAttach instead of a copy")
	}

	plain, err := reconcilerFor(t).buildCluster(aDocument(nil), theCluster)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Spec.MultiAttach != nil {
		t.Errorf("a document without multiAttach produced %+v", plain.Spec.MultiAttach)
	}
}

func TestTheDocumentSchemaHoldsAMultiAttachTemplateToItsRequirements(t *testing.T) {
	apiClient := apiServer(t)
	const requires = "multiAttach requires raidDevices"
	for _, tc := range []struct {
		name       string
		mutate     func(*simplyblockv1alpha2.ClusterDeploymentConfig)
		wantDenied string
	}{
		{name: "complete"},
		{name: "without raidDevices", mutate: func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Cluster.RaidDevices = nil
		}, wantDenied: requires},
		{name: "single node", mutate: func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Cluster.ClusterMode = simplyblockv1alpha2.ClusterModeSingle
		}, wantDenied: requires},
		{name: "dedicated journal device", mutate: func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Cluster.EnableJournalDevice = ptr.To(true)
		}, wantDenied: requires},
		{name: "NVMe devices", mutate: func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.NodeSets[0].Groups = groupsOf([]string{"worker-1", "worker-2"}, 1, nil)
		}, wantDenied: requires},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
				c.Name = ""
				c.GenerateName = "ma-"
				c.Namespace = "default"
				c.Spec.Approved = false
				c.Spec.Cluster.RaidDevices = ptr.To(true)
				c.Spec.Cluster.MultiAttach = multiAttachTemplate(1)
				c.Spec.NodeSets[0].Groups = blockGroupsOf([]string{"worker-1", "worker-2"}, 1)
				if tc.mutate != nil {
					tc.mutate(c)
				}
			})
			err := apiClient.Create(context.Background(), config)
			if tc.wantDenied != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantDenied) {
					t.Fatalf("create err = %v, want a refusal containing %q", err, tc.wantDenied)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			_ = apiClient.Delete(context.Background(), config)
		})
	}
}
