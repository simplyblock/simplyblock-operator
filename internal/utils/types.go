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
	BlkSize                int                   `json:"blk_size,omitempty"` // 512 or 4096
	PageSizeInBlocks       int                   `json:"page_size_in_blocks,omitempty"`
	CapWarn                int                   `json:"cap_warn,omitempty"`
	CapCrit                int                   `json:"cap_crit,omitempty"`
	ProvCapWarn            int                   `json:"prov_cap_warn,omitempty"`
	ProvCapCrit            int                   `json:"prov_cap_crit,omitempty"`
	DistrNdcs              int                   `json:"distr_ndcs,omitempty"`
	DistrNpcs              int                   `json:"distr_npcs,omitempty"`
	DistrBs                int                   `json:"distr_bs,omitempty"`
	DistrChunkBs           int                   `json:"distr_chunk_bs,omitempty"`
	HAType                 string                `json:"ha_type,omitempty"`
	QpairCount             int                   `json:"qpair_count,omitempty"`
	ClientQpairCount       int                   `json:"client_qpair_count,omitempty"`
	MaxQueueSize           int                   `json:"max_queue_size,omitempty"`
	InflightIOThreshold    int                   `json:"inflight_io_threshold,omitempty"`
	EnableNodeAffinity     bool                  `json:"enable_node_affinity,omitempty"`
	StrictNodeAntiAffinity bool                  `json:"strict_node_anti_affinity,omitempty"`
	IsSingleNode           bool                  `json:"is_single_node,omitempty"`
	Fabric                 string                `json:"fabric,omitempty"`
	CRName                 string                `json:"cr_name,omitempty"`
	CRNameSpace            string                `json:"cr_namespace,omitempty"`
	CRPlural               string                `json:"cr_plural,omitempty"`
	ClientDataIfname       string                `json:"client_data_ifname,omitempty"`
	MaxFaultTolerance      int                   `json:"max_fault_tolerance,omitempty"`
	NvmfBasePort           int                   `json:"nvmf_base_port,omitempty"`
	RpcBasePort            int                   `json:"rpc_base_port,omitempty"`
	SnodeApiPort           int                   `json:"snode_api_port,omitempty"`
	BackupConfig           *BackupConfig         `json:"backup_config,omitempty"`
	HashicorpVaultSettings *HashicorpVaultConfig `json:"hashicorp_vault_settings,omitempty"`
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

type StorageNodeAddParams struct {
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
	SpdkSystemMemory    string   `json:"spdk_sys_mem,omitempty"`
}
