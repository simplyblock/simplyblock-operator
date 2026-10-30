package utils

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
	SpdkSystemMemory    string   `json:"spdk_sys_mem,omitempty"`
	// FailureDomain assigns this node to a failure-domain group (integer ≥ 0).
	// Required when the cluster has EnableFailureDomain=true; nil (never sent)
	// otherwise. A pointer, not a plain int: domain 0 is a valid group index
	// (nodeFailureDomains allows >= 0), so a plain int with `omitempty` would
	// have silently dropped the field for every domain-0 node instead of
	// sending 0 -- the backend then saw no failure_domain at all and the
	// node never came online (2026-08-27 incident, found live redeploying
	// with domains numbered from 0).
	FailureDomain *int `json:"failure_domain,omitempty"`
	// Site is the replication site this node is added to, required on a sync
	// cluster and rejected otherwise. Wire key must match the add-node endpoint
	// (sbcli sync-replication branch).
	Site string `json:"site,omitempty"`
	// Expand signals that this node is being added to expand an already-active cluster.
	Expand bool `json:"expand,omitempty"`
	// ForceFormat wipes partitioned lblk devices at add-node time; node_configure.py
	// --force-format only marks them selectable, this flag does the actual wipe.
	ForceFormat bool `json:"force_format,omitempty"`
	// Networks are the networks the SPDK pod attaches to, with the address it
	// takes on each. Unset for a node on the host network, and then the request
	// is what it always was.
	Networks []NodeNetworkParams `json:"networks,omitempty"`
	// PodNetwork puts the SPDK pod on the pod network with no attachment, fronted
	// by a Service the control plane keeps. Exclusive with Networks.
	PodNetwork bool `json:"pod_network,omitempty"`
}

// NodeNetworkParams is one network the SPDK pod attaches to, as the control
// plane's node-add takes it.
type NodeNetworkParams struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace,omitempty"`
	Interface string   `json:"interface"`
	IPs       []string `json:"ips,omitempty"`
	// Resource is the extended resource a device plugin hands the interface's
	// device out under, from the definition's k8s.v1.cni.cncf.io/resourceName
	// annotation (an SR-IOV VF pool, for example). The SPDK pod requests one of
	// it, without which the plugin allocates nothing and Multus has no device to
	// attach. Not part of the Multus network selection element.
	Resource string `json:"resource,omitempty"`
}
