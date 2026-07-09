package utils

const (
	FinalizerPool                = "storage.simplyblock.io/pool-finalizer"
	FinalizerTask                = "storage.simplyblock.io/task-finalizer"
	FinalizerSnapshotReplication = "storage.simplyblock.io/snapshotreplication-finalizer"
	FinalizerStorageNodeSet      = "storage.simplyblock.io/storagenodeset-finalizer"
	FinalizerStorageCluster      = "storage.simplyblock.io/cluster-finalizer"

	ClusterActionActivate    = "activate"
	ClusterActionExpand      = "expand"
	ClusterActionShutdown    = "shutdown"
	ClusterActionStart       = "start"
	ClusterActionRestart     = "restart"
	ClusterActionNodeRecycle = "node-recycle"

	// NodeRecycle per-node phases
	NodeRecyclePhaseSnodeRefresh     = "snode-refresh"
	NodeRecyclePhaseSnodeRefreshWait = "snode-refresh-wait"
	NodeRecyclePhaseShuttingDown     = "shutting-down"
	NodeRecyclePhaseRestarting       = "restarting"
	NodeRecyclePhaseRebalancing      = "rebalancing"

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
	NodeStatusInShutdown  = "in_shutdown"
	NodeStatusInRestart   = "in_restart"
	NodeStatusUnreachable = "unreachable"

	ENDPOINT       = "http://simplyblock-webappapi:5000"
	CSIProvisioner = "csi.simplyblock.io"

	SecretNameStorageNodeSetAPITLS = "simplyblock-storage-node-api-tls"
	SecretNameSpdkProxyTLS         = "simplyblock-spdk-proxy-tls"

	// Webhook serving-certificate wiring. WebhookServiceName and
	// WebhookConfigurationName carry the kustomize namePrefix (simplyblock-operator-)
	// applied in config/default. The serving certificate is provisioned into
	// WebhookCertDir at runtime — self-signed via open-policy-agent/cert-controller,
	// or from the cert-manager-issued Secret when SB_TLS_PROVIDER=cert-manager.
	WebhookServiceName       = "simplyblock-operator-webhook-service"
	WebhookConfigurationName = "simplyblock-operator-mutating-webhook-configuration"
	WebhookServerCertSecret  = "webhook-server-cert"

	// WebhookCertDir is where the serving cert (tls.crt/tls.key) is written and
	// watched. This is the default location controller-runtime's webhook server
	// (sigs.k8s.io/controller-runtime/pkg/webhook) reads from when CertDir is
	// unset: filepath.Join(os.TempDir(), "k8s-webhook-server", "serving-certs").
	// We keep the same path (rather than a bespoke one) so both the cert-controller
	// rotator and the webhook-server certwatcher agree without extra flags, and so
	// the config/default/manager_webhook_patch.yaml emptyDir mount lines up.
	WebhookCertDir = "/tmp/k8s-webhook-server/serving-certs"

	AnnotationTLSSecretRevision = "storage.simplyblock.io/tls-secret-revision"

	LabelFDBClusterName = "foundationdb.org/fdb-cluster-name"
	LabelSpdkProxyRole  = "simplyblock-storage-node"
)
