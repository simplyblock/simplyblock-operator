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
