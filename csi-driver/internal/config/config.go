// Package config holds the CSI driver's parsed command-line configuration. It
// is a leaf: the flags are bound in cmd/, and every other package reads the
// struct without reaching back for anything else.
package config

// Config stores parsed command line parameters
type Config struct {
	DriverName    string
	DriverVersion string
	Endpoint      string
	NodeID        string

	IsControllerServer bool
	IsNodeServer       bool

	// Link to the operator. The driver dials out and holds the connection, and the
	// operator issues its RPCs back down it. Disabled by default, since the driver
	// serves CSI whether or not it is linked.
	LinkEnabled bool
	// LinkHubAddress is the operator's link endpoint, `host:port`.
	LinkHubAddress string
	// LinkCAFile signs the operator's serving certificate. Empty uses the
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
