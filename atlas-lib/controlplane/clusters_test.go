package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

// clusterDTO is a cluster as a control plane with the workload names reports it.
const clusterDTO = `{"id":"` + testCluster + `","name":"prod","nqn":"nqn.2023-02.io.simplyblock:` + testCluster + `",` +
	`"status":"active","is_re_balancing":false,"is_shrinking":true,"block_size":512,"distr_ndcs":2,"distr_npcs":1,` +
	`"ha":true,"utilization_critical":0,"utilization_warning":0,"provisioned_capacity_critical":0,` +
	`"provisioned_capacity_warning":0,"node_affinity":false,"anti_affinity":false,"enable_failure_domain":false,` +
	`"device_mode":"nvme","inline_checksum":false,"atomic_4k":false,"secret":"s3cret","tls_enabled":true,` +
	`"max_fault_tolerance":1,"backup_enabled":false,"capacity":{"date":0,"size_free":0,"size_prov":0,` +
	`"size_total":0,"size_used":0,"size_util":0},"snode_api_service":"sb-prod-node-api",` +
	`"spdk_proxy_service":"sb-prod-spdk-proxy","spdk_proxy_tls_secret":"sb-prod-spdk-proxy-tls",` +
	`"node_label_suffix":"-kqv"}`

func prodWorkloadNames() ClusterWorkloadNames {
	return ClusterWorkloadNames{
		NodeAPIService: "sb-prod-node-api", SpdkProxyService: "sb-prod-spdk-proxy",
		SpdkProxyTLSSecret: "sb-prod-spdk-proxy-tls", NodeLabelSuffix: "-kqv",
	}
}

func TestClientCreateClusterSendsTheSpecsFieldNames(t *testing.T) {
	var body map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/clusters/" {
			t.Errorf("request = %s %s, want POST /api/v2/clusters/", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode the body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(clusterDTO))
	})

	got, err := c.CreateCluster(context.Background(), CreateClusterParams{
		Name: "prod", NDCS: 2, NPCS: 1, MaxSubsys: 20, SpdkVCPUCount: 8, ClientDataNIC: "eth1",
		CRName: "prod", CRNamespace: "sb", CRPlural: "storageclusters", DeviceMode: "nvme",
		WorkloadNames: prodWorkloadNames(),
		Backup:        &ClusterBackup{Endpoint: "http://minio:9000", AccessKeyID: "AKID", SecretAccessKey: "SK"},
		VaultBaseURL:  "https://vault:8200",
	})
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]any{
		"name": "prod", "distr_ndcs": float64(2), "distr_npcs": float64(1), "max_subsys": float64(20),
		"spdk_vcpu_count": float64(8), "client_data_nic": "eth1", "cr_namespace": "sb", "device_mode": "nvme",
		"snode_api_service": "sb-prod-node-api", "spdk_proxy_service": "sb-prod-spdk-proxy",
		"spdk_proxy_tls_secret": "sb-prod-spdk-proxy-tls", "node_label_suffix": "-kqv",
	} {
		if body[key] != want {
			t.Errorf("body[%q] = %v, want %v", key, body[key], want)
		}
	}
	backup, _ := body["backup_config"].(map[string]any)
	credentials, _ := backup["credentials"].(map[string]any)
	if backup["endpoint"] != "http://minio:9000" || credentials["access_key_id"] != "AKID" {
		t.Errorf("backup_config = %v", body["backup_config"])
	}
	vault, _ := body["hashicorp_vault_settings"].(map[string]any)
	if vault["base_url"] != "https://vault:8200" {
		t.Errorf("hashicorp_vault_settings = %v", body["hashicorp_vault_settings"])
	}

	if got.ID != testCluster || got.Secret != "s3cret" || got.NDCS != 2 || got.NPCS != 1 || !got.Shrinking {
		t.Errorf("CreateCluster = %+v", got)
	}
	if got.WorkloadNames != prodWorkloadNames() {
		t.Errorf("WorkloadNames = %+v, want %+v", got.WorkloadNames, prodWorkloadNames())
	}
}

// The control plane's refusal keeps its status and body, which is what an
// event about the refusal quotes.
func TestClientCreateClusterRefusalCarriesTheAnswer(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"a cluster of that name exists"}`))
	})

	_, err := c.CreateCluster(context.Background(), CreateClusterParams{Name: "prod", MaxSubsys: 20, SpdkVCPUCount: 8})

	var refusal *StatusError
	if !errors.As(err, &refusal) || refusal.StatusCode != http.StatusConflict {
		t.Fatalf("err = %v, want a 409 StatusError", err)
	}
	if !errors.Is(err, errs.ErrAlreadyExists) {
		t.Errorf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestClientGetClusterReadsItsWorkloadNames(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(clusterDTO))
	})

	got, err := c.GetCluster(context.Background(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != testCluster || got.Name != "prod" || got.Status != "active" || got.MaxFaultTolerance != 1 {
		t.Errorf("GetCluster = %+v", got)
	}
	if got.WorkloadNames != prodWorkloadNames() {
		t.Errorf("WorkloadNames = %+v, want %+v", got.WorkloadNames, prodWorkloadNames())
	}
}

// A control plane that predates the workload names sends none, which reads as
// empty names rather than an error: an empty name is the constant one.
func TestClientGetClusterFromAControlPlaneWithoutWorkloadNames(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + testCluster + `","nqn":"nqn.x","status":"active","distr_ndcs":1,"distr_npcs":1}`))
	})

	got, err := c.GetCluster(context.Background(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkloadNames != (ClusterWorkloadNames{}) {
		t.Errorf("WorkloadNames = %+v, want none", got.WorkloadNames)
	}
}

func TestClientGetClusterRefusesABodyWithoutAnID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active"}`))
	})

	if _, err := c.GetCluster(context.Background(), testCluster); !errors.Is(err, errs.ErrInvalidResponse) {
		t.Errorf("err = %v, want ErrInvalidResponse", err)
	}
}

func TestClientListClusters(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + clusterDTO + "]"))
	})

	got, err := c.ListClusters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "prod" || got[0].WorkloadNames != prodWorkloadNames() {
		t.Errorf("ListClusters = %+v", got)
	}
}

func TestClientClusterCapacityIsTheNewestSample(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/clusters/"+testCluster+"/capacity" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"date":10,"size_total":100,"size_used":10,"size_prov":50},` +
			`{"date":30,"size_total":300,"size_used":30,"size_prov":150},` +
			`{"date":20,"size_total":200,"size_used":20,"size_prov":100}]`))
	})

	got, err := c.ClusterCapacity(context.Background(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.TotalBytes != 300 || got.UsedBytes != 30 || got.ProvisionedBytes != 150 {
		t.Errorf("ClusterCapacity = %+v, want the sample dated 30", got)
	}
}

func TestClientClusterCapacityWithNoSampleIsNone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	got, err := c.ClusterCapacity(context.Background(), testCluster)
	if err != nil || got != nil {
		t.Errorf("ClusterCapacity = %+v, %v, want none", got, err)
	}
}
