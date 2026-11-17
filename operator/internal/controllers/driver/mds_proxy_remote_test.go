// The mds-proxy's listener ports on a cluster whose storage nodes are not its
// own: a managed consumer, whose storage cluster is known only to a remote
// control plane and is listed in simplyblock-csi-secret-v2.

package driver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const remoteClusterID = "2b201eb1-15b6-4bb6-a341-23acde78bb6c"

// remoteControlPlane answers a storage-node listing for remoteClusterID with
// nodes serving their volumes on lvolPorts, and records the bearer tokens it saw.
func remoteControlPlane(t *testing.T, lvolPorts ...int) (*httptest.Server, *[]string) {
	t.Helper()
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if r.URL.Path != "/api/v2/clusters/"+remoteClusterID+"/storage-nodes/" {
			http.NotFound(w, r)
			return
		}
		nodes := make([]map[string]any, 0, len(lvolPorts))
		for i, port := range lvolPorts {
			nodes = append(nodes, map[string]any{
				"id": "77777777-7777-7777-7777-77777777777" + string(rune('0'+i)), "cluster_id": remoteClusterID,
				"hostname": "node", "status": "online", "mgmt_ip": "10.0.0.5", "lvols": 0, "lvols_max": 0,
				"device_count": 0, "capacity": map[string]any{}, "secondary_node_id": nil,
				"lvol_subsys_port": port,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodes)
	}))
	t.Cleanup(srv.Close)
	return srv, &tokens
}

// csiCredentials is simplyblock-csi-secret-v2 naming one cluster at endpoint.
func csiCredentials(endpoint, secret string) *corev1.Secret {
	payload, _ := json.Marshal(map[string]any{"clusters": []map[string]any{{
		"cluster_id": remoteClusterID, "cluster_endpoint": endpoint, "cluster_secret": secret,
	}}})
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "simplyblock", Name: "simplyblock-csi-secret-v2"},
		Data:       map[string][]byte{"secret.json": payload},
	}
}

func publishedPorts(t *testing.T, c client.Client) string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "simplyblock", Name: "simplyblock-csi-mds-proxy-ports"}, &cm); err != nil {
		t.Fatalf("the ports ConfigMap: %v", err)
	}
	return cm.Data["ports"]
}

// Regression: 2026-11-11-mds-proxy-remote-ports. On a managed consumer the
// storage nodes are in another Kubernetes cluster, so there is no StorageNode
// to read a listener port from and the ports ConfigMap stayed empty. The proxy
// then refused every connect from the metadata server to a storage node as
// not an address of this node on no storage listener port, and every pNFS
// export ended Degraded after its assembly timed out.
func TestTheMDSProxyIsToldTheListenerPortsOfARemoteStorageCluster(t *testing.T) {
	srv, tokens := remoteControlPlane(t, 4428, 4430)
	d := withMDS(testDriver("simplyblock"))
	c := reconcileDriver(t, d, csiCredentials(srv.URL, "cluster-secret"))

	if got := publishedPorts(t, c); got != "4428\n4430\n" {
		t.Errorf("ports = %q, want the remote storage nodes' listener ports", got)
	}
	for _, tok := range *tokens {
		if tok != "cluster-secret" {
			t.Errorf("the control plane was called with token %q, want the cluster's secret", tok)
		}
	}
}

// A control plane that cannot be reached must not take ports away from a
// metadata server already serving through them: the ports published before
// stay until an answer says otherwise.
func TestAnUnreachableControlPlaneKeepsThePublishedPorts(t *testing.T) {
	srv, _ := remoteControlPlane(t)
	url := srv.URL
	srv.Close()

	d := withMDS(testDriver("simplyblock"))
	published := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: d.Namespace, Name: "simplyblock-csi-mds-proxy-ports"},
		Data:       map[string]string{"ports": "4428\n"},
	}
	c := reconcileDriver(t, d, csiCredentials(url, "cluster-secret"), published)

	if got := publishedPorts(t, c); got != "4428\n" {
		t.Errorf("ports = %q, want the published 4428 kept while the control plane is unreachable", got)
	}
}
