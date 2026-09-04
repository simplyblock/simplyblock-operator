package utils

const (
	FinalizerStoragePool       = "storage.simplyblock.io/storagepool-finalizer"
	FinalizerTask              = "storage.simplyblock.io/task-finalizer"
	FinalizerStorageNodeSet    = "storage.simplyblock.io/storagenodeset-finalizer"
	FinalizerStorageCluster    = "storage.simplyblock.io/cluster-finalizer"
	FinalizerStorageClusterOps = "storage.simplyblock.io/storageclusterops-finalizer"
	FinalizerReplicationPolicy = "storage.simplyblock.io/replicationpolicy-finalizer"
	FinalizerReplicationPair   = "storage.simplyblock.io/replicationpair-finalizer"
	FinalizerReplicationSlot   = "storage.simplyblock.io/replicationslot-finalizer"

	// AnnotationReplicationPolicy is the annotation key on StorageClass or PVC that
	// opts volumes into a named ReplicationPolicy CR.
	AnnotationReplicationPolicy = "storage.simplyblock.io/replication-policy"

	// ReplicationOps scope values.
	ReplicationOpsScopeTarget = "target"
	ReplicationOpsScopePolicy = "policy"
	ReplicationOpsScopeVolume = "volume"

	// ReplicationBackendStateReplicating is the backend API state string for a
	// volume that is actively replicating snapshots to the target cluster.
	ReplicationBackendStateReplicating = "replicating"

	ClusterActionActivate           = "activate"
	ClusterActionExpand             = "expand"
	ClusterActionShutdown           = "shutdown"
	ClusterActionStart              = "start"
	ClusterActionRestart            = "restart"
	ClusterActionNodeRollingRestart = "node-rolling-restart"

	// StorageNode action names.
	NodeActionShutdown = "shutdown"
	NodeActionRestart  = "restart"
	NodeActionSuspend  = "suspend"
	NodeActionResume   = "resume"
	NodeActionRemove   = "remove"
	NodeActionMigrate  = "migrate"

	// NodeRollingRestart per-node phases
	NodeRollingRestartPhaseSnodeRefresh     = "snode-refresh"
	NodeRollingRestartPhaseSnodeRefreshWait = "snode-refresh-wait"
	NodeRollingRestartPhaseShuttingDown     = "shutting-down"
	NodeRollingRestartPhaseRestarting       = "restarting"
	NodeRollingRestartPhaseRebalancing      = "rebalancing"

	ActionStateRunning = "running"
	ActionStateSuccess = "success"
	ActionStateFailed  = "failed"

	TaskStateDone = "done"

	ClusterStatusActive    = "active"
	ClusterStatusSuspended = "suspended"
	ClusterStatusUnready   = "unready"

	ClusterPhaseInitializing = "Initializing"
	ClusterPhaseReady        = "Ready"

	NodeStatusOnline      = "online"
	NodeStatusOffline     = "offline"
	NodeStatusSuspended   = "suspended"
	NodeStatusInCreation  = "in_creation"
	NodeStatusTimeout     = "timeout"
	NodeStatusInShutdown  = "in_shutdown"
	NodeStatusInRestart   = "in_restart"
	NodeStatusUnreachable = "unreachable"
	NodeStatusRemoved     = "removed"

	ENDPOINT       = "http://simplyblock-webappapi:5000"
	CSIProvisioner = "csi.simplyblock.io"

	SecretNameStorageNodeSetAPITLS = "simplyblock-storage-node-api-tls"
	SecretNameSpdkProxyTLS         = "simplyblock-spdk-proxy-tls"

	// Webhook serving-certificate wiring. WebhookServiceName and
	// WebhookConfigurationName carry the Kustomize namePrefix (simplyblock-operator-)
	// applied in config/default. The serving certificate is provisioned into
	// WebhookCertDir at runtime — self-signed via open-policy-agent/cert-controller,
	// or from the cert-manager-issued Secret when SB_TLS_PROVIDER=cert-manager.
	WebhookServiceName                 = "simplyblock-operator-webhook-service"
	WebhookConfigurationName           = "simplyblock-operator-mutating-webhook-configuration"
	WebhookValidatingConfigurationName = "simplyblock-operator-validating-webhook-configuration"
	WebhookServerCertSecret            = "webhook-server-cert"

	// WebhookCertDir is where the serving cert (tls.crt/tls.key) is written and
	// watched. This is the default location controller-runtime's webhook server
	// (sigs.k8s.io/controller-runtime/pkg/webhook) reads from when CertDir is
	// unset: filepath.Join(os.TempDir(), "k8s-webhook-server", "serving-certs").
	// We keep the same path (rather than a bespoke one) so both the cert-controller
	// rotator and the webhook-server certwatcher agree without extra flags, and so
	// the config/default/manager_webhook_patch.yaml emptyDir mount lines up.
	WebhookCertDir = "/tmp/k8s-webhook-server/serving-certs"

	// Aggregated metrics API wiring. MetricsAPIServiceName and
	// MetricsAPIServiceObject carry the Kustomize namePrefix
	// (simplyblock-operator-) applied in config/default, except that an APIService
	// object's name is fixed by Kubernetes as <version>.<group> and takes no
	// prefix. The serving certificate is provisioned into
	// metricsapi.CertDir at runtime by the same cert-controller rotator the
	// webhook uses, which also injects the CA bundle into the APIService.
	MetricsAPIServiceName    = "simplyblock-operator-metrics-apiserver"
	MetricsAPIServiceObject  = "v1alpha1.metrics.simplyblock.io"
	MetricsAPIServerCertName = "metrics-apiserver-cert"

	// DefaultPrometheusURL is where the chart deploys Prometheus. Two things
	// read simplyblock's exported metrics through it: the rebalancer's placement
	// signals, and the aggregated metrics API's capacity samples. The address is
	// named once here rather than spelled out at each of them.
	DefaultPrometheusURL = "http://simplyblock-prometheus:9090"

	AnnotationTLSSecretRevision = "storage.simplyblock.io/tls-secret-revision"

	LabelFDBClusterName = "foundationdb.org/fdb-cluster-name"
	LabelSpdkProxyRole  = "simplyblock-storage-node"
)
