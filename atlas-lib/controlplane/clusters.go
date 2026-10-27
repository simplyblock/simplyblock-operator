// The cluster calls: creating a cluster, reading one, listing them, and its
// capacity. They live here, beside the other resources' calls, so that a cluster
// is created and read through the same generated, spec-validated client as
// everything inside it, and a field the control plane renames or drops fails
// here rather than reaching the operator as a zero.

package controlplane

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/ptr"
)

// Cluster is a simplyblock cluster as the control plane reports it.
type Cluster struct {
	ID                string
	Name              string
	NQN               string
	Secret            string
	Status            string
	Rebalancing       bool
	Shrinking         bool
	NDCS              int
	NPCS              int
	MaxFaultTolerance int
	WorkloadNames     ClusterWorkloadNames
	// SyncReplication is whether the cluster spans two sites, every write
	// landing on both before it is acknowledged.
	SyncReplication bool
	// LeaderSite is the site the cluster record names as leader; empty when
	// none is recorded.
	LeaderSite string
	// LostSite is the site recorded as out of service, empty while both are
	// in, and LostSiteState how far its drain or fence got.
	LostSite      string
	LostSiteState string
}

// ClusterWorkloadNames are the Services a cluster's storage nodes are reached
// through, the SPDK proxy's certificate Secret, and the suffix on a node's
// hostname. A control plane that predates them reports none, and an empty
// name means the constant one.
type ClusterWorkloadNames struct {
	NodeAPIService     string
	SpdkProxyService   string
	SpdkProxyTLSSecret string
	NodeLabelSuffix    string
}

// ClusterBackup is where a cluster's backups go.
type ClusterBackup struct {
	Endpoint        string
	BucketName      string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SnapshotBackups *bool
	WithCompression *bool
	SecondaryTarget *int
}

// CreateClusterParams are the inputs for creating a cluster. MaxSubsys and
// SpdkVCPUCount are required by the control plane, and every other zero value
// is left to its default.
type CreateClusterParams struct {
	Name                string
	CapWarn             int
	CapCrit             int
	ProvCapWarn         int
	ProvCapCrit         int
	NDCS                int
	NPCS                int
	EnableNodeAffinity  bool
	Fabric              string
	CRName              string
	CRNamespace         string
	CRPlural            string
	ClientDataNIC       string
	NvmfBasePort        int
	RPCBasePort         int
	SnodeAPIPort        int
	Backup              *ClusterBackup
	VaultBaseURL        string
	EnableFailureDomain bool
	SpdkVCPUCount       int
	HugepagesMemBytes   int64
	MaxSubsys           int
	InlineChecksum      bool
	Atomic4K            bool
	DeviceMode          string
	// SingleNode creates a cluster of one storage node, which is also created
	// without HA.
	SingleNode    bool
	WorkloadNames ClusterWorkloadNames
	// SyncReplication creates a cluster whose writes land on both of its
	// sites before they are acknowledged. The backend accepts it at create
	// only.
	SyncReplication bool
	// LeaderSite names the creating site the leader of a sync-replicated
	// cluster; empty leaves it unset.
	LeaderSite string
}

// ClusterCapacityReading is one capacity sample of a cluster, in bytes.
type ClusterCapacityReading struct {
	TotalBytes       int64
	UsedBytes        int64
	ProvisionedBytes int64
}

// CreateCluster creates a cluster and returns it as the control plane recorded
// it, which is where a caller learns whether the workload names were kept: a
// control plane that predates them accepts the body and drops them.
func (c *Client) CreateCluster(ctx context.Context, params CreateClusterParams) (Cluster, error) {
	what := fmt.Sprintf("create cluster %q", params.Name)
	resp, err := c.api.ClustersCreateApiV2ClustersPostWithResponse(ctx, nil, clusterParamsOf(params))
	if err != nil {
		return Cluster{}, fmt.Errorf("%s: %w", what, err)
	}
	// The create endpoint answers with the full ClusterDTO, which the spec
	// leaves untyped, so it is decoded here.
	d, err := decodeBody[cpapi.ClusterDTO](what, resp.StatusCode(), resp.Body)
	if err != nil {
		return Cluster{}, err
	}
	return clusterFromDTO(d), nil
}

// GetCluster returns one cluster by id. It wraps errs.ErrNotFound when the
// cluster does not exist.
func (c *Client) GetCluster(ctx context.Context, clusterID string) (Cluster, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return Cluster{}, err
	}
	resp, err := c.api.ClustersDetailApiV2ClustersClusterIdGetWithResponse(ctx, cluster, nil)
	if err != nil {
		return Cluster{}, fmt.Errorf("get cluster %s: %w", clusterID, err)
	}
	d, err := payload("cluster "+clusterID, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return Cluster{}, err
	}
	return clusterFromDTO(*d), nil
}

// ListClusters returns every cluster of the control plane.
func (c *Client) ListClusters(ctx context.Context) ([]Cluster, error) {
	resp, err := c.api.ClustersListApiV2ClustersGetWithResponse(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	ds, err := payload("list clusters", resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]Cluster, 0, len(*ds))
	for _, d := range *ds {
		out = append(out, clusterFromDTO(d))
	}
	return out, nil
}

// ClusterCapacity returns the cluster's newest capacity sample, or nil when it
// has none. The endpoint answers with a list of dated samples, which the spec
// leaves untyped.
func (c *Client) ClusterCapacity(ctx context.Context, clusterID string) (*ClusterCapacityReading, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersCapacityApiV2ClustersClusterIdCapacityGetWithResponse(ctx, cluster, nil)
	if err != nil {
		return nil, fmt.Errorf("get the capacity of cluster %s: %w", clusterID, err)
	}
	samples, err := decodeBody[[]cpapi.CapacityStatDTO]("capacity of cluster "+clusterID, resp.StatusCode(), resp.Body)
	if err != nil || len(samples) == 0 {
		return nil, err
	}
	newest := samples[0]
	for _, sample := range samples[1:] {
		if sample.Date > newest.Date {
			newest = sample
		}
	}
	return &ClusterCapacityReading{
		TotalBytes:       int64(newest.SizeTotal),
		UsedBytes:        int64(newest.SizeUsed),
		ProvisionedBytes: int64(newest.SizeProv),
	}, nil
}

func clusterFromDTO(d cpapi.ClusterDTO) Cluster {
	return Cluster{
		ID:                d.Id.String(),
		Name:              ptr.From(d.Name, ""),
		NQN:               d.Nqn,
		Secret:            ptr.From(d.Secret, ""),
		Status:            string(d.Status),
		Rebalancing:       d.IsReBalancing,
		Shrinking:         ptr.BoolFromOrFalse(d.IsShrinking),
		NDCS:              d.DistrNdcs,
		NPCS:              d.DistrNpcs,
		MaxFaultTolerance: d.MaxFaultTolerance,
		WorkloadNames: ClusterWorkloadNames{
			NodeAPIService:     d.SnodeApiService,
			SpdkProxyService:   d.SpdkProxyService,
			SpdkProxyTLSSecret: d.SpdkProxyTlsSecret,
			NodeLabelSuffix:    d.NodeLabelSuffix,
		},
		SyncReplication: ptr.BoolFromOrFalse(d.SyncReplication),
		LeaderSite:      ptr.From(d.LeaderSite, ""),
		LostSite:        ptr.From(d.LostSite, ""),
		LostSiteState:   ptr.From(d.LostSiteState, ""),
	}
}

// clusterParamsOf is the create body. A zero value is left out, so the control
// plane applies its own default for it.
func clusterParamsOf(p CreateClusterParams) cpapi.ClusterParams {
	body := cpapi.ClusterParams{
		MaxSubsys:              p.MaxSubsys,
		SpdkVcpuCount:          p.SpdkVCPUCount,
		Name:                   nonEmpty(p.Name),
		CapWarn:                nonZero(p.CapWarn),
		CapCrit:                nonZero(p.CapCrit),
		ProvCapWarn:            nonZero(p.ProvCapWarn),
		ProvCapCrit:            nonZero(p.ProvCapCrit),
		DistrNdcs:              nonZero(p.NDCS),
		DistrNpcs:              nonZero(p.NPCS),
		EnableNodeAffinity:     isTrue(p.EnableNodeAffinity),
		Fabric:                 nonEmpty(p.Fabric),
		CrName:                 nonEmpty(p.CRName),
		CrNamespace:            nonEmpty(p.CRNamespace),
		CrPlural:               nonEmpty(p.CRPlural),
		ClientDataNic:          nonEmpty(p.ClientDataNIC),
		NvmfBasePort:           nonZero(p.NvmfBasePort),
		RpcBasePort:            nonZero(p.RPCBasePort),
		SnodeApiPort:           nonZero(p.SnodeAPIPort),
		EnableFailureDomain:    isTrue(p.EnableFailureDomain),
		HugepagesMem:           nonZero(int(p.HugepagesMemBytes)),
		InlineChecksum:         isTrue(p.InlineChecksum),
		Atomic4k:               isTrue(p.Atomic4K),
		IsSingleNode:           isTrue(p.SingleNode),
		SyncReplication:        isTrue(p.SyncReplication),
		LeaderSite:             nonEmpty(p.LeaderSite),
		SnodeApiService:        nonEmpty(p.WorkloadNames.NodeAPIService),
		SpdkProxyService:       nonEmpty(p.WorkloadNames.SpdkProxyService),
		SpdkProxyTlsSecret:     nonEmpty(p.WorkloadNames.SpdkProxyTLSSecret),
		NodeLabelSuffix:        nonEmpty(p.WorkloadNames.NodeLabelSuffix),
		HashicorpVaultSettings: vaultSettingsOf(p.VaultBaseURL),
		BackupConfig:           backupConfigOf(p.Backup),
	}
	if p.DeviceMode != "" {
		body.DeviceMode = ptr.To(cpapi.ClusterParamsDeviceMode(p.DeviceMode))
	}
	// A single-node cluster has no second node to place a secondary on. Left
	// to the control plane's default, 'ha', every volume of it would ask for
	// one.
	if p.SingleNode {
		body.HaType = ptr.To(cpapi.ClusterParamsHaTypeSingle)
	}
	return body
}

func vaultSettingsOf(baseURL string) *cpapi.HashicorpVaultSettings {
	if baseURL == "" {
		return nil
	}
	return &cpapi.HashicorpVaultSettings{BaseUrl: ptr.To(baseURL)}
}

func backupConfigOf(b *ClusterBackup) *cpapi.UnresolvedBackupConfig {
	if b == nil {
		return nil
	}
	config := &cpapi.UnresolvedBackupConfig{
		Endpoint:        nonEmpty(b.Endpoint),
		BucketName:      nonEmpty(b.BucketName),
		Region:          nonEmpty(b.Region),
		SnapshotBackups: b.SnapshotBackups,
		WithCompression: b.WithCompression,
	}
	if b.AccessKeyID != "" || b.SecretAccessKey != "" {
		config.Credentials = &cpapi.S3Credentials{
			AccessKeyId:     nonEmpty(b.AccessKeyID),
			SecretAccessKey: nonEmpty(b.SecretAccessKey),
		}
	}
	if b.SecondaryTarget != nil {
		config.SecondaryTarget = ptr.To(cpapi.SecondaryTarget(*b.SecondaryTarget))
	}
	return config
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonZero(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func isTrue(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}
