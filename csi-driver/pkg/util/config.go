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

package util

const (
	cfgRPCTimeoutSeconds = 120
)

// Config stores parsed command line parameters
type Config struct {
	DriverName    string
	DriverVersion string
	Endpoint      string
	NodeID        string

	IsControllerServer bool
	IsNodeServer       bool

	// Link to the operator. The driver dials out and holds the connection; the
	// operator issues its RPCs back down it. Disabled by default — the driver
	// serves CSI whether or not it is linked.
	LinkEnabled bool
	// LinkHubAddress is the operator's link endpoint, "host:port".
	LinkHubAddress string
	// LinkCAFile signs the operator's serving certificate; empty uses the
	// system roots.
	LinkCAFile string
	// LinkServerName overrides the name verified against that certificate.
	LinkServerName string
	// LinkTokenFile is the projected ServiceAccount token presented to the
	// operator. It must be projected with the audience the operator expects.
	LinkTokenFile string

	// PodUID identifies this process lifetime to the operator, so a restarted
	// pod supersedes the session its predecessor left behind. From the
	// downward API (metadata.uid).
	PodUID string
	// PodName is this pod's name, which is how a controller plugin is
	// identified on the link (a node plugin is identified by NodeID).
	PodName string
}
