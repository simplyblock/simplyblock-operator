package utils

type BackupConfig struct {
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	LocalEndpoint   string `json:"local_endpoint,omitempty"`
	SnapshotBackups *bool  `json:"snapshot_backups,omitempty"`
	WithCompression *bool  `json:"with_compression,omitempty"`
	SecondaryTarget *int32 `json:"secondary_target,omitempty"`
	LocalTesting    *bool  `json:"local_testing,omitempty"`
}

type HashicorpVaultConfig struct {
	BaseURL string `json:"base_url,omitempty"`
}

type ClusterAddParams struct {
	Name                   string                `json:"name"`
	CapWarn                int                   `json:"cap_warn,omitempty"`
	CapCrit                int                   `json:"cap_crit,omitempty"`
	ProvCapWarn            int                   `json:"prov_cap_warn,omitempty"`
	ProvCapCrit            int                   `json:"prov_cap_crit,omitempty"`
	DistrNdcs              int                   `json:"distr_ndcs,omitempty"`
	DistrNpcs              int                   `json:"distr_npcs,omitempty"`
	DistrBs                int                   `json:"distr_bs,omitempty"`
	DistrChunkBs           int                   `json:"distr_chunk_bs,omitempty"`
	EnableNodeAffinity     bool                  `json:"enable_node_affinity,omitempty"`
	Fabric                 string                `json:"fabric,omitempty"`
	CRName                 string                `json:"cr_name,omitempty"`
	CRNameSpace            string                `json:"cr_namespace,omitempty"`
	CRPlural               string                `json:"cr_plural,omitempty"`
	ClientDataIfname       string                `json:"client_data_ifname,omitempty"`
	NvmfBasePort           int                   `json:"nvmf_base_port,omitempty"`
	RpcBasePort            int                   `json:"rpc_base_port,omitempty"`
	SnodeApiPort           int                   `json:"snode_api_port,omitempty"`
	BackupConfig           *BackupConfig         `json:"backup_config,omitempty"`
	HashicorpVaultSettings *HashicorpVaultConfig `json:"hashicorp_vault_settings,omitempty"`
	// EnableFailureDomain opts the cluster into failure-domain mode.
	// Wire key must match the /api/v2/clusters/ endpoint — verify against sbcli before release.
	EnableFailureDomain bool  `json:"enable_failure_domain,omitempty"`
	SpdkVcpuCount       int   `json:"spdk_vcpu_count,omitempty"`
	HugepagesMem        int64 `json:"hugepages_mem,omitempty"`
	MaxSubsys           uint  `json:"max_subsys,omitempty"`
	// InlineChecksum enables inline CRC checksum validation for silent-data-error protection.
	// Requires backend support on the /api/v2/clusters/ endpoint (sbcli PR #1250, not merged
	// as of this writing) — sending it against an unpatched backend is a silent no-op.
	InlineChecksum bool `json:"inline_checksum,omitempty"`
	// Atomic4k declares 4K write atomicity on devices with a <4K logical block size. Only
	// meaningful when InlineChecksum is true. Same backend-support caveat as InlineChecksum.
	Atomic4k bool `json:"atomic_4k,omitempty"`
}

type ClusterUpdateParams struct {
	CapWarn                int    `json:"cap_warn,omitempty"`
	CapCrit                int    `json:"cap_crit,omitempty"`
	ProvCapWarn            int    `json:"prov_cap_warn,omitempty"`
	ProvCapCrit            int    `json:"prov_cap_crit,omitempty"`
	QoSClasses             string `json:"qos_classes,omitempty"`
	LogDelInterval         string `json:"log_del_interval,omitempty"`
	MetricsRetentionPeriod string `json:"metrics_retention_period,omitempty"`
	ClientQpairCount       int    `json:"client_qpair_count,omitempty"`
	IncludeStats           bool   `json:"include_stats,omitempty"`
	StatsHistoryInSeconds  int    `json:"stats_history_in_seconds,omitempty"`
	IncludeEventLog        bool   `json:"include_event_log,omitempty"`
	EventLogEntries        int    `json:"event_log_entries,omitempty"`
}

type ReplicationAddParams struct {
	TargetCluster string `json:"snapshot_replication_target_cluster"`
	Timeout       int    `json:"snapshot_replication_timeout,omitempty"`
	TargetPool    string `json:"target_pool,omitempty"`
}

type PoolAddParams struct {
	Name          string `json:"name"`
	PoolMax       int64  `json:"pool_max,omitempty"`
	VolumeMaxSize int64  `json:"volume_max_size,omitempty"`
	MaxRwIOPS     int    `json:"max_rw_iops,omitempty"`
	MaxRwMB       int    `json:"max_rw_mbytes,omitempty"`
	MaxRMB        int    `json:"max_r_mbytes,omitempty"`
	MaxWMB        int    `json:"max_w_mbytes,omitempty"`
	DHCHAP        bool   `json:"dhchap,omitempty"`
	CRName        string `json:"cr_name,omitempty"`
	CRNameSpace   string `json:"cr_namespace,omitempty"`
	CRPlural      string `json:"cr_plural,omitempty"`
}

type PoolUpdateParams struct {
	Name            string `json:"name,omitempty"`
	PoolMax         int64  `json:"pool_max,omitempty"`
	VolumeMaxSize   int64  `json:"lvol_max,omitempty"`
	MaxRwIOPS       int    `json:"max_rw_iops,omitempty"`
	MaxRwMB         int    `json:"max_rw_mbytes,omitempty"`
	MaxRMB          int    `json:"max_r_mbytes,omitempty"`
	MaxWMB          int    `json:"max_w_mbytes,omitempty"`
	LvolCRName      string `json:"lvols_cr_name,omitempty"`
	LvolCRNameSpace string `json:"lvols_cr_namespace,omitempty"`
	LvolCRPlural    string `json:"lvols_cr_plural,omitempty"`
}

type StorageNodeSetAddParams struct {
	NodeAddress         string   `json:"node_address"`
	InterfaceName       string   `json:"interface_name"`
	SPDKImage           string   `json:"spdk_image,omitempty"`
	SPDKProxyImage      string   `json:"spdk_proxy_image,omitempty"`
	SPDKDebug           bool     `json:"spdk_debug"`
	IdDeviceByNQN       bool     `json:"id_device_by_nqn"`
	DataNics            []string `json:"data_nics,omitempty"`
	Namespace           string   `json:"namespace"`
	JMPercent           int      `json:"jm_percent"`
	Partitions          int      `json:"partitions"`
	IOBufSmallPoolCount int      `json:"iobuf_small_pool_count,omitempty"`
	IOBufLargePoolCount int      `json:"iobuf_large_pool_count,omitempty"`
	HaJMCount           int      `json:"ha_jm_count,omitempty"`
	CRName              string   `json:"cr_name,omitempty"`
	CRNameSpace         string   `json:"cr_namespace,omitempty"`
	CRPlural            string   `json:"cr_plural,omitempty"`
	Format4K            bool     `json:"format_4k,omitempty"`
	EnableTestDevice    bool     `json:"enable_test_device,omitempty"`
	SpdkSystemMemory    string   `json:"spdk_sys_mem,omitempty"`
	// FailureDomain assigns this node to a failure-domain group (integer ≥ 1).
	// Required when the cluster has EnableFailureDomain=true; omit (zero value) otherwise.
	FailureDomain int `json:"failure_domain,omitempty"`
	// Expand signals that this node is being added to expand an already-active cluster.
	Expand bool `json:"expand,omitempty"`
}
