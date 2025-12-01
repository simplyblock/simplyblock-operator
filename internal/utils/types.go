package utils

type ClusterAddParams struct {
	Name                   string `json:"name,omitempty"`
	BlkSize                int    `json:"blk_size"` // 512 or 4096
	PageSizeInBlocks       int    `json:"page_size_in_blocks"`
	CapWarn                int    `json:"cap_warn"`
	CapCrit                int    `json:"cap_crit"`
	ProvCapWarn            int    `json:"prov_cap_warn"`
	ProvCapCrit            int    `json:"prov_cap_crit"`
	DistrNdcs              int    `json:"distr_ndcs"`
	DistrNpcs              int    `json:"distr_npcs"`
	DistrBs                int    `json:"distr_bs"`
	DistrChunkBs           int    `json:"distr_chunk_bs"`
	HAType                 string `json:"ha_type"`
	QpairCount             int    `json:"qpair_count"`
	MaxQueueSize           int    `json:"max_queue_size"`
	InflightIOThreshold    int    `json:"inflight_io_threshold"`
	EnableNodeAffinity     bool   `json:"enable_node_affinity"`
	StrictNodeAntiAffinity bool   `json:"strict_node_anti_affinity"`
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
	PoolMax       int    `json:"pool_max"`
	VolumeMaxSize int    `json:"volume_max_size"`
	MaxRwIOPS     int    `json:"max_rw_iops"`
	MaxRwMB       int    `json:"max_rw_mbytes"`
	MaxRMB        int    `json:"max_r_mbytes"`
	MaxWMB        int    `json:"max_w_mbytes"`
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
