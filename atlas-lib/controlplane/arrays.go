// The storage array of a multi-attach cluster: registering it with the control
// plane and reading the registration back.
//
// A multi-attach cluster runs on LUNs of an external array. The control plane
// creates, maps, grows, and fences them itself through the array plugin named
// here, so this is the only place the operator hands it the array's address
// and credentials. The credentials travel in the registration request and are
// never returned: the control plane wraps them with the cluster's key store, or
// reads them from an external secret named by CredentialRef instead.

package controlplane

import (
	"context"
	"fmt"
	"sort"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// ArrayRegistration is what the control plane needs to reach a cluster's array.
type ArrayRegistration struct {
	// Plugin is the array plugin's name, e.g., pure-flasharray or netapp-ontap.
	Plugin string
	// Endpoint is the array's management URL.
	Endpoint string
	// Options are the plugin's settings (protocol, pool, CA bundle, ...).
	Options map[string]string
	// Credentials are the array's login fields. Leave empty when
	// CredentialRef names an external secret, or when the plugin needs none.
	Credentials map[string]string
	// CredentialRef names a secret kept outside simplyblock, which the control
	// plane reads and never stores: {"kind": "vault-kv", "mount", "path"} or
	// {"kind": "kmip-secret", "uid"}.
	CredentialRef map[string]string
	// LunSizeBytes is the size of the LUN created for each node added to the
	// cluster; zero creates none.
	LunSizeBytes int64
	// JournalOnLUN creates each node's journal as a LUN of its own next to its
	// data LUN, presented to that node only; false keeps the journal on a
	// local disk or partition the node names.
	JournalOnLUN bool
	// JournalSizeBytes is the size of a journal LUN; zero lets the control
	// plane size it (3% of the data LUN, at least 2 GiB).
	JournalSizeBytes int64
	// JournalOptions are plugin settings for the journal LUNs, applied over
	// Options (a faster pool).
	JournalOptions map[string]string
	// AllowLocalKeyStore opts in (or out) of keeping Credentials in the local
	// key store of a cluster without an external KMS; nil leaves it as it is.
	AllowLocalKeyStore *bool
}

// Array is the control plane's record of a registered array. It carries no
// credential, only whether there are any and what kind of reference holds
// them.
type Array struct {
	ID           string
	Plugin       string
	Endpoint     string
	Options      map[string]string
	LunSizeBytes int64
	// JournalOnLUN, JournalSizeBytes, and JournalOptions are the journal
	// placement as the control plane holds it.
	JournalOnLUN     bool
	JournalSizeBytes int64
	JournalOptions   map[string]string
	HasCredentials   bool
	// CredentialKind is managed, vault-kv, kmip-secret, or empty.
	CredentialKind string
	// Protocols are the protocols the plugin reported it can serve, when the
	// answer carried its capabilities.
	Protocols []string
}

// String renders the record for logs and events.
func (a Array) String() string {
	return fmt.Sprintf("array %s (%s at %s, credentials: %s)", a.ID, a.Plugin, a.Endpoint,
		map[bool]string{true: a.CredentialKind, false: "none"}[a.HasCredentials])
}

func arrayFromDTO(d cpapi.ArrayDTO) Array {
	out := Array{
		ID:             d.Id,
		Plugin:         d.Plugin,
		Endpoint:       d.Endpoint,
		Options:        map[string]string{},
		LunSizeBytes:   int64(d.LunSize),
		HasCredentials: d.HasCredentials,
	}
	for k, v := range d.Options {
		out.Options[k] = fmt.Sprint(v)
	}
	out.JournalOnLUN = d.Journal != nil && *d.Journal == "lun"
	if d.JournalLunSize != nil {
		out.JournalSizeBytes = int64(*d.JournalLunSize)
	}
	if d.JournalOptions != nil && len(*d.JournalOptions) > 0 {
		out.JournalOptions = map[string]string{}
		for k, v := range *d.JournalOptions {
			out.JournalOptions[k] = fmt.Sprint(v)
		}
	}
	if d.CredentialRef != nil {
		if kind, ok := (*d.CredentialRef)["kind"].(string); ok {
			out.CredentialKind = kind
		}
	}
	if d.Capabilities != nil {
		if protocols, ok := (*d.Capabilities)["protocols"].([]any); ok {
			for _, p := range protocols {
				out.Protocols = append(out.Protocols, fmt.Sprint(p))
			}
			sort.Strings(out.Protocols)
		}
	}
	return out
}

// RegisterArray registers the cluster's array, or registers it again, which is
// how its endpoint, options, or credentials change. The control plane connects
// to the array with them before saving anything, so a refusal means nothing
// was changed; it carries the control plane's reason.
func (c *Client) RegisterArray(ctx context.Context, clusterID string, reg ArrayRegistration) (Array, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return Array{}, err
	}
	body := cpapi.ArrayParams{Plugin: reg.Plugin}
	if reg.Endpoint != "" {
		body.Endpoint = &reg.Endpoint
	}
	if len(reg.Options) > 0 {
		opts := make(map[string]interface{}, len(reg.Options))
		for k, v := range reg.Options {
			opts[k] = v
		}
		body.Options = &opts
	}
	if len(reg.Credentials) > 0 {
		creds := make(map[string]string, len(reg.Credentials))
		for k, v := range reg.Credentials {
			creds[k] = v
		}
		body.Credentials = &creds
	}
	if len(reg.CredentialRef) > 0 {
		ref := make(map[string]interface{}, len(reg.CredentialRef))
		for k, v := range reg.CredentialRef {
			ref[k] = v
		}
		body.CredentialRef = &ref
	}
	if reg.LunSizeBytes > 0 {
		size := int(reg.LunSizeBytes)
		body.LunSize = &size
	}
	journal := cpapi.Local
	if reg.JournalOnLUN {
		journal = cpapi.Lun
		if reg.JournalSizeBytes > 0 {
			size := int(reg.JournalSizeBytes)
			body.JournalLunSize = &size
		}
		if len(reg.JournalOptions) > 0 {
			opts := make(map[string]interface{}, len(reg.JournalOptions))
			for k, v := range reg.JournalOptions {
				opts[k] = v
			}
			body.JournalOptions = &opts
		}
	}
	body.Journal = &journal
	if reg.AllowLocalKeyStore != nil {
		allow := *reg.AllowLocalKeyStore
		body.AllowLocalKmsCredentials = &allow
	}
	what := fmt.Sprintf("register array %s for cluster %s", reg.Plugin, clusterID)
	resp, err := c.api.ClustersArraysCreateApiV2ClustersClusterIdArraysPostWithResponse(ctx, cluster, body)
	if err != nil {
		// The transport error never contains the body, so never the credentials.
		return Array{}, fmt.Errorf("%s: %w", what, err)
	}
	d, err := payload(what, resp.JSON201, resp.StatusCode(), resp.Body)
	if err != nil {
		return Array{}, err
	}
	return arrayFromDTO(*d), nil
}

// ListArrays returns the cluster's registered arrays: none or one.
func (c *Client) ListArrays(ctx context.Context, clusterID string) ([]Array, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return nil, err
	}
	what := "list arrays of cluster " + clusterID
	resp, err := c.api.ClustersArraysListApiV2ClustersClusterIdArraysGetWithResponse(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	ds, err := payload(what, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]Array, 0, len(*ds))
	for _, d := range *ds {
		out = append(out, arrayFromDTO(d))
	}
	return out, nil
}

// LUNGrowth is the outcome of growing a cluster's LUNs to a size.
type LUNGrowth struct {
	// Grown reports whether the cluster's devices grew; false with no
	// errors means nothing was left to grow.
	Grown bool
	// Errors names what could not be grown, by device id; empty when every
	// LUN has the size and every device took it over.
	Errors map[string]string
	// Reason is the control plane's explanation when nothing grew.
	Reason string
}

// GrowLUNs grows every LUN the array plugin created for the cluster to
// sizeBytes at the array, waits until every node sees the size, and grows the
// cluster's devices onto it. A LUN already that large is left alone, so a call
// that failed half-way is simply repeated. Nodes added later get LUNs of the
// size.
func (c *Client) GrowLUNs(ctx context.Context, clusterID string, sizeBytes int64) (LUNGrowth, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return LUNGrowth{}, err
	}
	size := int(sizeBytes)
	params := &cpapi.ClustersGrowDevicesApiV2ClustersClusterIdGrowDevicesPostParams{LunSize: &size}
	what := fmt.Sprintf("grow the LUNs of cluster %s to %d bytes", clusterID, sizeBytes)
	resp, err := c.api.ClustersGrowDevicesApiV2ClustersClusterIdGrowDevicesPostWithResponse(ctx, cluster, params)
	if err != nil {
		return LUNGrowth{}, fmt.Errorf("%s: %w", what, err)
	}
	d, err := payload(what, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return LUNGrowth{}, err
	}
	return LUNGrowth{Grown: d.Grown, Errors: d.Errors, Reason: d.Reason}, nil
}
