// Tests of the operator API's answer for an endpoint without a fixture: the
// console reads it as an empty collection, and the mock records the request so
// /mockctl/info names the fixture still missing.

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The console's API client (api.jsx, preq) unwraps a results envelope or takes
// the body as it is. The volume list of a cluster is such a request, and the
// page broke on {"status":"success","data":[]}: r.map is not a function.
func TestAnUnmockedOperatorEndpointAnswersAnEmptyCollection(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")
	res, err := http.Get(ts.URL + "/operator/v1/proposed/lvols?scope=clusters&scopeId=x")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var body struct {
		Results *[]any `json:"results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Results == nil || len(*body.Results) != 0 {
		t.Fatalf("an unmocked endpoint answered %s, want {\"results\": []}", raw)
	}

	info, err := http.Get(ts.URL + "/mockctl/info")
	if err != nil {
		t.Fatal(err)
	}
	defer info.Body.Close()
	infoRaw, _ := io.ReadAll(info.Body)
	if !strings.Contains(string(infoRaw), "proposed/lvols") {
		t.Fatalf("the unmocked request is not recorded: %s", infoRaw)
	}
}
