/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"os"

	"k8s.io/klog"

	"github.com/spdk/spdk-csi/pkg/spdk"
	"github.com/spdk/spdk-csi/pkg/util"
)

const (
	driverName    = "csi.simplyblock.io"
	driverVersion = "0.1.0"
)

var conf = util.Config{
	DriverVersion: driverVersion,
}

func init() {
	flag.StringVar(&conf.DriverName, "drivername", driverName, "Name of the driver")
	flag.StringVar(&conf.Endpoint, "endpoint", "unix://tmp/spdkcsi.sock", "CSI endpoint")
	flag.StringVar(&conf.NodeID, "nodeid", "", "node id")
	flag.BoolVar(&conf.IsControllerServer, "controller", false, "Start controller server")
	flag.BoolVar(&conf.IsNodeServer, "node", false, "Start node server")

	flag.BoolVar(&conf.LinkEnabled, "link", false,
		"Dial the operator and hold the connection open, so it can query this pod's storage state")
	flag.StringVar(&conf.LinkHubAddress, "link-hub-address", "",
		"The operator's link endpoint, host:port")
	flag.StringVar(&conf.LinkCAFile, "link-ca-file", "",
		"CA bundle signing the operator's link certificate; empty uses the system roots")
	flag.StringVar(&conf.LinkServerName, "link-server-name", "",
		"Name to verify against the operator's link certificate, when it differs from the address dialled")
	flag.StringVar(&conf.LinkTokenFile, "link-token-file",
		"/var/run/secrets/simplyblock.io/link/token",
		"Projected ServiceAccount token presented to the operator; must carry the audience it expects")
	flag.StringVar(&conf.PodUID, "pod-uid", os.Getenv("POD_UID"),
		"This pod's UID (downward API), so a restart supersedes the previous link session")
	flag.StringVar(&conf.PodName, "pod-name", os.Getenv("POD_NAME"),
		"This pod's name (downward API), which identifies a controller plugin on the link")

	klog.InitFlags(nil)
	if err := flag.Set("logtostderr", "true"); err != nil {
		klog.Exitf("failed to set logtostderr flag: %v", err)
	}
	flag.Parse()
}

func main() {
	klog.Infof("Starting SPDK-CSI driver: %v version: %v", conf.DriverName, driverVersion)

	spdk.Run(&conf)

	os.Exit(0)
}
