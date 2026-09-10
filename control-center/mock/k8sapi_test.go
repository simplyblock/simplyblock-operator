package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, dataset string) (*server, *httptest.Server) {
	t.Helper()
	sc, ok := scenarioByName(dataset)
	if !ok {
		t.Fatalf("unknown dataset %s", dataset)
	}
	srv := newServer(sc, 1234, "simplyblock", "", "global")
	ts := httptest.NewServer(srv.routes(""))
	t.Cleanup(ts.Close)
	return srv, ts
}

func getJSON(t *testing.T, url string, into any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("GET %s: decode: %v", url, err)
		}
	}
	return resp
}

func TestDiscovery(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")

	var groups struct {
		Groups []struct{ Name string } `json:"groups"`
	}
	getJSON(t, ts.URL+"/apis", &groups)
	found := map[string]bool{}
	for _, g := range groups.Groups {
		found[g.Name] = true
	}
	for _, want := range []string{sbGroup, ramenGroup, "apps", "snapshot.storage.k8s.io"} {
		if !found[want] {
			t.Errorf("group %s missing from /apis", want)
		}
	}

	var rl struct {
		Resources []struct{ Name string } `json:"resources"`
	}
	getJSON(t, ts.URL+"/apis/storage.simplyblock.io/v1alpha1", &rl)
	names := map[string]bool{}
	for _, r := range rl.Resources {
		names[r.Name] = true
	}
	for _, want := range []string{"storageclusters", "storagenodes", "storagedeviceops"} {
		if !names[want] {
			t.Errorf("resource %s missing from discovery", want)
		}
	}
}

func TestListAndGetGenerated(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")

	var list struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	getJSON(t, ts.URL+"/apis/storage.simplyblock.io/v1alpha1/namespaces/sb-sc-cluster1/storagenodes", &list)
	if list.Kind != "StorageNodeList" || len(list.Items) != 3 {
		t.Fatalf("want StorageNodeList with 3 items, got %s with %d", list.Kind, len(list.Items))
	}

	// every device must point at an existing node
	var devs struct{ Items []map[string]any }
	getJSON(t, ts.URL+"/apis/storage.simplyblock.io/v1alpha1/storagedevices", &devs)
	if len(devs.Items) == 0 {
		t.Fatal("no devices generated")
	}
	for _, d := range devs.Items {
		nodeRef := getStr(d, "spec.nodeRef")
		resp := getJSON(t, ts.URL+"/apis/storage.simplyblock.io/v1alpha1/namespaces/sb-sc-cluster1/storagenodes/"+nodeRef, nil)
		if resp.StatusCode != 200 {
			t.Errorf("device %s references missing node %s", getStr(d, "metadata.name"), nodeRef)
		}
	}

	// a proposed kind answers an empty list, not a 404
	var empty struct{ Items []map[string]any }
	resp := getJSON(t, ts.URL+"/apis/storage.simplyblock.io/v1alpha1/storagedeviceops", &empty)
	if resp.StatusCode != 200 || len(empty.Items) != 0 {
		t.Fatalf("proposed kind should return an empty list, got %d items (status %d)", len(empty.Items), resp.StatusCode)
	}

	// unknown resource is a k8s Status 404
	resp = getJSON(t, ts.URL+"/apis/storage.simplyblock.io/v1alpha1/doesnotexist", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown resource should 404, got %d", resp.StatusCode)
	}
}

func TestCreatePatchDeleteRoundtrip(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")
	base := ts.URL + "/apis/storage.simplyblock.io/v1alpha1/namespaces/sb-sc-cluster1/storagenodeops"

	body := `{"metadata":{"name":"restart-sn-01"},"spec":{"action":"restart","storageNodeRef":"sn-01"}}`
	resp, err := http.Post(base, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("create: want 201 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// merge-patch spec.abort — the UI's abort gesture
	req, _ := http.NewRequest(http.MethodPatch, base+"/restart-sn-01", strings.NewReader(`{"spec":{"abort":true}}`))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	presp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var patched map[string]any
	json.NewDecoder(presp.Body).Decode(&patched)
	presp.Body.Close()
	if ab, _ := getPath(patched, "spec.abort").(bool); !ab {
		t.Fatalf("patch did not set spec.abort: %v", patched["spec"])
	}
	if getStr(patched, "spec.action") != "restart" {
		t.Fatal("merge patch clobbered unrelated spec fields")
	}

	dreq, _ := http.NewRequest(http.MethodDelete, base+"/restart-sn-01", nil)
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != 200 {
		t.Fatalf("delete: want 200 got %d", dresp.StatusCode)
	}
	if r := getJSON(t, base+"/restart-sn-01", nil); r.StatusCode != 404 {
		t.Fatalf("get after delete: want 404 got %d", r.StatusCode)
	}
}

func TestWatchStream(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")
	base := "/apis/storage.simplyblock.io/v1alpha1/namespaces/sb-sc-cluster1/storageclusterops"

	resp, err := http.Get(ts.URL + base + "?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	done := make(chan map[string]any, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) == nil {
				done <- ev
				return
			}
		}
	}()

	// give the watch a moment to register, then create
	time.Sleep(100 * time.Millisecond)
	body := `{"metadata":{"name":"watch-op"},"spec":{"action":"restart","clusterRef":"sb-primary"}}`
	cresp, err := http.Post(ts.URL+base, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	cresp.Body.Close()

	select {
	case ev := <-done:
		if ev["type"] != "ADDED" {
			t.Fatalf("want ADDED, got %v", ev["type"])
		}
		obj := ev["object"].(map[string]any)
		if getStr(obj, "metadata.name") != "watch-op" {
			t.Fatalf("wrong object on stream: %v", obj["metadata"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no watch event within 3s")
	}
}

func TestPrometheusEndpoints(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")

	var instant struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []any  `json:"result"`
		} `json:"data"`
	}
	getJSON(t, ts.URL+"/api/v1/query?query=sb_node_read_iops", &instant)
	if instant.Status != "success" || instant.Data.ResultType != "vector" || len(instant.Data.Result) != 3 {
		t.Fatalf("instant query: %+v", instant)
	}

	now := time.Now().Unix()
	url := fmt.Sprintf("%s/api/v1/query_range?query=rate(sb_node_write_bytes[5m])&start=%d&end=%d&step=60", ts.URL, now-3600, now)
	var rng struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Values [][]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	getJSON(t, url, &rng)
	if rng.Data.ResultType != "matrix" || len(rng.Data.Result) == 0 || len(rng.Data.Result[0].Values) < 50 {
		t.Fatalf("range query: %+v", rng.Data.ResultType)
	}

	// the same instants must be reproducible (seeded)
	var again struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/api/v1/query?query=sb_node_read_iops&time=%d", ts.URL, now)
	getJSON(t, u, &again)
	first := again.Data.Result[0].Value[1]
	getJSON(t, u, &again)
	if again.Data.Result[0].Value[1] != first {
		t.Fatal("prometheus values are not deterministic for a fixed time")
	}
}

func TestMockctl(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")

	var info struct {
		Dataset string         `json:"dataset"`
		Objects map[string]int `json:"objects"`
	}
	getJSON(t, ts.URL+"/mockctl/info", &info)
	if info.Dataset != "small-healthy" || info.Objects["storage.simplyblock.io/storagenodes"] != 3 {
		t.Fatalf("info: %+v", info)
	}

	resp, err := http.Post(ts.URL+"/mockctl/reset?dataset=medium-degraded&seed=7", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	getJSON(t, ts.URL+"/mockctl/info", &info)
	if info.Dataset != "medium-degraded" || info.Objects["storage.simplyblock.io/storagenodes"] != 8 {
		t.Fatalf("after reset: %+v", info)
	}
}
