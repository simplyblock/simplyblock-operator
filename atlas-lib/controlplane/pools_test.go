package controlplane

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

func TestClientListAndFindPools(t *testing.T) {
	const p1 = `{"id":"` + testPool + `","cluster_id":"` + testCluster + `","name":"pool1",` +
		`"max_size":1000,"capacity":{},"max_r_mbytes":0,"max_rw_iops":0,"max_rw_mbytes":0,` +
		`"max_w_mbytes":0,"volume_max_size":0,"status":"active"}`
	const p2 = `{"id":"55555555-5555-5555-5555-555555555555","cluster_id":"` + testCluster + `",` +
		`"name":"pool2","max_size":2000,"capacity":{},"max_r_mbytes":0,"max_rw_iops":0,` +
		`"max_rw_mbytes":0,"max_w_mbytes":0,"volume_max_size":0,"status":"active"}`

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + p1 + "," + p2 + "]"))
	})

	pools, err := c.ListStoragePools(context.Background(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(pools))
	}
	if pools[0].ID != testPool || pools[0].Name != "pool1" || pools[0].MaxSizeBytes != 1000 {
		t.Errorf("pools[0] = %+v", pools[0])
	}

	got, err := c.StoragePoolByName(context.Background(), testCluster, "pool2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "pool2" || got.MaxSizeBytes != 2000 {
		t.Errorf("PoolByName(pool2) = %+v", got)
	}

	if _, err := c.StoragePoolByName(context.Background(), testCluster, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("PoolByName(missing) err = %v, want ErrNotFound", err)
	}
}

func TestClientGetPoolNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.GetStoragePool(context.Background(), testCluster, testPool); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetPool err = %v, want ErrNotFound", err)
	}
}

func TestClientCreateStoragePool(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + testPool + `","cluster_id":"` + testCluster + `","name":"newpool",` +
			`"max_size":5000,"capacity":{},"max_r_mbytes":0,"max_rw_iops":0,"max_rw_mbytes":0,` +
			`"max_w_mbytes":0,"volume_max_size":0,"status":"active"}`))
	})
	p, err := c.CreateStoragePool(context.Background(), testCluster, CreateStoragePoolParams{Name: "newpool", MaxSizeBytes: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "newpool" || p.ID != testPool || p.MaxSizeBytes != 5000 {
		t.Errorf("pool = %+v", p)
	}
}
