// The operator pod's log names the operations the simulator drives, the way
// the real operator's reconcile lines carry the object they are about, so the
// console's activity view has lines to show against the mock.
package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// operatorPodLog reads the operator pod's log through the Kubernetes API the
// way the console does: the pod found by its app label, then pods/log with
// timestamps.
func operatorPodLog(t *testing.T, base string) string {
	t.Helper()
	var pods struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		} `json:"items"`
	}
	getJSON(t, base+"/k8s/api/v1/pods?labelSelector=app%3Dsimplyblock-operator", &pods)
	if len(pods.Items) != 1 {
		t.Fatalf("expected one operator pod, got %d", len(pods.Items))
	}
	p := pods.Items[0].Metadata
	resp, err := http.Get(base + "/k8s/api/v1/namespaces/" + p.Namespace + "/pods/" + p.Name + "/log?tailLines=2000&timestamps=true")
	if err != nil {
		t.Fatalf("GET log: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET log: %d %s", resp.StatusCode, body)
	}
	return string(body)
}

func TestOperatorLogNamesTheOperationsTheSimulatorDrives(t *testing.T) {
	srv, ts := newTestServer(t, "small-healthy")
	d := testDef()
	seedNode(srv.store, srv.namespace, "sn-log")
	srv.store.Create(d, srv.namespace, map[string]any{
		"metadata": map[string]any{"name": "op-log-restart"},
		"spec":     map[string]any{"action": "Restart", "nodeRef": "sn-log"},
	})
	resp, err := http.Post(ts.URL+"/mockctl/advance?ticks=12", "", nil)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	resp.Body.Close()

	body := operatorPodLog(t, ts.URL)
	for _, want := range []string{"op-log-restart", "StorageNodeOps", "Departing"} {
		if !strings.Contains(body, want) {
			t.Errorf("the operator log does not mention %q:\n%s", want, body)
		}
	}
	// every line carries the kubelet timestamp the console strips, then the
	// operator's own line
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if !strings.Contains(line, "op-log-restart") {
			continue
		}
		if sp := strings.IndexByte(line, ' '); sp <= 0 || !strings.Contains(line[:sp], "T") {
			t.Errorf("line does not start with a timestamp: %q", line)
		}
		if !strings.Contains(line, `"StorageNodeOps": {"name": "op-log-restart"`) {
			t.Errorf("line does not carry the object key the way controller-runtime writes it: %q", line)
		}
	}
}
