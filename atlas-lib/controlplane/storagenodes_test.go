package controlplane

import (
	"context"
	"net/http"
	"testing"
)

const testNode = "77777777-7777-7777-7777-777777777777"

func TestClientListStorageNodes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"` + testNode + `","cluster_id":"` + testCluster + `","hostname":"node1",` +
			`"status":"online","mgmt_ip":"10.0.0.5","lvols":3,"lvols_max":100,"device_count":4,` +
			`"capacity":{},"secondary_node_id":null}]`))
	})
	nodes, err := c.ListStorageNodes(context.Background(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	n := nodes[0]
	if n.ID != testNode || n.Hostname != "node1" || n.Status != "online" ||
		n.MgmtIP != "10.0.0.5" || n.Lvols != 3 || n.MaxLvols != 100 || n.DeviceCount != 4 {
		t.Errorf("node = %+v", n)
	}
	if n.SecondaryNodeID != "" {
		t.Errorf("SecondaryNodeID = %q, want empty", n.SecondaryNodeID)
	}
}

func TestClientGetStorageNode(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"` + testNode + `","cluster_id":"` + testCluster + `","hostname":"node1",` +
			`"status":"online","mgmt_ip":"10.0.0.5","lvols":0,"lvols_max":0,"device_count":0,` +
			`"capacity":{},"secondary_node_id":null}`))
	})
	n, err := c.GetStorageNode(context.Background(), testCluster, testNode)
	if err != nil {
		t.Fatal(err)
	}
	if n.Hostname != "node1" || n.ID != testNode {
		t.Errorf("node = %+v", n)
	}
}

func TestClientListStorageNodeNICs(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"ID":"nic-1","Device name":"eth0","Address":"10.10.10.5",` +
			`"Net type":"tcp","Status":"online"}]`))
	})
	nics, err := c.ListStorageNodeNICs(context.Background(), testCluster, testNode)
	if err != nil {
		t.Fatal(err)
	}
	if len(nics) != 1 || nics[0].Address != "10.10.10.5" || nics[0].NetType != "tcp" ||
		nics[0].Device != "eth0" || nics[0].ID != "nic-1" {
		t.Errorf("nics = %+v", nics)
	}
}
