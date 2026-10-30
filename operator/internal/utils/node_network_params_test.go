// The node-add body for a node on a network, serialized with and without the
// attachment list.

package utils

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTheAddBodyCarriesTheNetworksWhenThereAreAny(t *testing.T) {
	body, err := json.Marshal(StorageNodeSetAddParams{
		NodeAddress: "10.0.0.1:5000",
		Networks: []NodeNetworkParams{{
			Name: "fabric", Namespace: "infra", Interface: "data0",
			IPs: []string{"10.10.10.101/24"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := `"networks":[{"name":"fabric","namespace":"infra","interface":"data0","ips":["10.10.10.101/24"]}]`
	if !strings.Contains(string(body), want) {
		t.Fatalf("body = %s, want it to contain %s", body, want)
	}
}

func TestTheAddBodyWithoutNetworksIsTheOneItAlwaysWas(t *testing.T) {
	body, err := json.Marshal(StorageNodeSetAddParams{NodeAddress: "10.0.0.1:5000"})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(body), "networks") {
		t.Fatalf("body = %s, want no networks key", body)
	}
}
