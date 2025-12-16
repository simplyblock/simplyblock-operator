package utils

type ClusterAddParams struct {
	Name                   string `json:"name"`
	BlkSize                int    `json:"blk_size,omitempty"` // 512 or 4096
	PageSizeInBlocks       int    `json:"page_size_in_blocks,omitempty"`
	CapWarn                int    `json:"cap_warn,omitempty"`
	CapCrit                int    `json:"cap_crit,omitempty"`
	ProvCapWarn            int    `json:"prov_cap_warn,omitempty"`
	ProvCapCrit            int    `json:"prov_cap_crit,omitempty"`
	DistrNdcs              int    `json:"distr_ndcs,omitempty"`
	DistrNpcs              int    `json:"distr_npcs,omitempty"`
	DistrBs                int    `json:"distr_bs,omitempty"`
	DistrChunkBs           int    `json:"distr_chunk_bs,omitempty"`
	HAType                 string `json:"ha_type,omitempty"`
	QpairCount             int    `json:"qpair_count,omitempty"`
	MaxQueueSize           int    `json:"max_queue_size,omitempty"`
	InflightIOThreshold    int    `json:"inflight_io_threshold,omitempty"`
	EnableNodeAffinity     bool   `json:"enable_node_affinity,omitempty"`
	StrictNodeAntiAffinity bool   `json:"strict_node_anti_affinity,omitempty"`
	IsSingleNode           bool   `json:"is_single_node,omitempty"`
	Fabric                 string `json:"fabric,omitempty"`
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

type PoolAddParams struct {
	Name          string `json:"name"`
	PoolMax       int    `json:"pool_max,omitempty"`
	VolumeMaxSize int    `json:"volume_max_size,omitempty"`
	MaxRwIOPS     int    `json:"max_rw_iops,omitempty"`
	MaxRwMB       int    `json:"max_rw_mbytes,omitempty"`
	MaxRMB        int    `json:"max_r_mbytes,omitempty"`
	MaxWMB        int    `json:"max_w_mbytes,omitempty"`
}

type PoolUpdateParams struct {
	Name          string `json:"name,omitempty"`
	PoolMax       int    `json:"pool_max,omitempty"`
	VolumeMaxSize int    `json:"lvol_max,omitempty"`
	MaxRwIOPS     int    `json:"max_rw_iops,omitempty"`
	MaxRwMB       int    `json:"max_rw_mbytes,omitempty"`
	MaxRMB        int    `json:"max_r_mbytes,omitempty"`
	MaxWMB        int    `json:"max_w_mbytes,omitempty"`
}

type StorageNodeAddParams struct {
	NodeAddress         string   `json:"node_address"`
	InterfaceName       string   `json:"interface_name"`
	SPDKImage           string   `json:"spdk_image,omitempty"`
	SPDKDebug           bool     `json:"spdk_debug"`
	DataNics            []string `json:"data_nics,omitempty"`
	Namespace           string   `json:"namespace"`
	JMPercent           int      `json:"jm_percent"`
	Partitions          int      `json:"partitions"`
	IOBufSmallPoolCount int      `json:"iobuf_small_pool_count,omitempty"`
	IOBufLargePoolCount int      `json:"iobuf_large_pool_count,omitempty"`
	HaJMCount           int      `json:"ha_jm_count,omitempty"`
}
