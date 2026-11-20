package main

// Several Kubernetes contexts in one mock: each under `/k8s/<name>/` with its
// own world, the first one also at the root, and config.js naming them with
// their labels for the console's fleet dashboard.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextsServeSeparateWorlds(t *testing.T) {
	specs, err := parseContexts("dev=small-healthy,uat=medium-degraded:stage=uat;tenant=acme")
	if err != nil {
		t.Fatal(err)
	}
	srv := newFleetServer(specs, 1234, "simplyblock", "", "global")
	ts := httptest.NewServer(srv.routes("../"))
	t.Cleanup(ts.Close)

	var local, dev, uat objList
	getJSON(t, ts.URL+v2+"/storageclusters", &local)
	getJSON(t, ts.URL+"/k8s/dev"+v2+"/storageclusters", &dev)
	getJSON(t, ts.URL+"/k8s/uat"+v2+"/storageclusters", &uat)
	if len(local.Items) == 0 || len(dev.Items) != 1 || len(uat.Items) != 1 {
		t.Fatalf("want one cluster per context, got local %d, dev %d, uat %d", len(local.Items), len(dev.Items), len(uat.Items))
	}
	if getStr(dev.Items[0], "status.uuid") != getStr(local.Items[0], "status.uuid") {
		t.Errorf("the first context is the local one: root uuid %s, dev uuid %s", getStr(local.Items[0], "status.uuid"), getStr(dev.Items[0], "status.uuid"))
	}
	if getStr(dev.Items[0], "status.uuid") == getStr(uat.Items[0], "status.uuid") {
		t.Errorf("dev and uat share a cluster uuid; the worlds are not separate")
	}
	var uatNodes objList
	getJSON(t, ts.URL+"/k8s/uat"+v2+"/storagenodes", &uatNodes)
	if len(uatNodes.Items) != 8 {
		t.Errorf("uat runs medium-degraded with 8 nodes, got %d", len(uatNodes.Items))
	}

	resp, err := http.Get(ts.URL + "/config.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	cfg := string(buf[:n])
	for _, want := range []string{`"name":"dev"`, `"name":"uat"`, `"stage":"dev"`, `"stage":"uat"`, `"tenant":"acme"`, `"k8sBase":"/k8s/uat"`, `"local":true`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config.js lacks %s:\n%s", want, cfg)
		}
	}
	if _, err := parseContexts("api=small-healthy"); err == nil {
		t.Errorf("a reserved name must be refused")
	}
}

