// The names of every object the managed install applies, and the ports they
// answer on.
//
// They are constants rather than derivations from the ControlPlane's name, and
// that is deliberate: every one of them is the name the Helm chart already
// rendered, and a running deployment refers to them from places this operator
// does not control. The management API's Service name is in the CSI driver's
// configuration and in the Prometheus scrape configuration. The
// FoundationDBCluster's name is in the cluster file every workload mounts. The
// shared account's name is in the SB_K8S_ADMIN_SERVICE_ACCOUNTS list the control
// plane checks tokens against. Deriving them would rename all of it on the first
// install.
//
// The ControlPlane is a singleton named `simplyblock` (design-controlplane.md
// §3.1), so there is never a second set of these to collide with.

package controlplane

// SingletonName is the fixed name of the ControlPlane object. The controller
// ignores any other name, which is enforcement by convention rather than by the
// API server (design-controlplane.md §3.1).
const SingletonName = "simplyblock"

// The management API and the services that share its image and its account.
const (
	// ComponentWebAPI is the management API: the endpoint every controller in
	// this operator reaches, and the one component whose absence is an outage.
	ComponentWebAPI = "simplyblock-webappapi"

	// ComponentTasks is the task runner. Its work is queued, so a task runner at
	// zero defers what is waiting rather than dropping it.
	ComponentTasks = "simplyblock-tasks"

	// ComponentMonitoring is the pool of per-subject monitors and collectors.
	ComponentMonitoring = "simplyblock-monitoring"

	// ComponentAdminControl is the administrative surface: a long-lived pod with
	// the control plane's tooling on it and no service of its own.
	ComponentAdminControl = "simplyblock-admin-control"
)

// The store and the FoundationDB half.
const (
	// ComponentMinio is the object store the control plane keeps long-term data
	// in.
	ComponentMinio = "simplyblock-minio"

	// ComponentFDBCluster is the FoundationDBCluster, whose readiness is that
	// resource's own report rather than a replica count: a cluster at two of
	// three coordinators is serving and a count cannot say so.
	ComponentFDBCluster = "simplyblock-fdb-cluster"

	// ComponentFDBOperator is the FoundationDB operator's own workload, applied
	// only where the Kubernetes cluster does not already run one.
	ComponentFDBOperator = "simplyblock-fdb-controller-manager"

	// ComponentFDBExporter turns FoundationDB's status JSON into Prometheus
	// metrics.
	ComponentFDBExporter = "simplyblock-fdb-exporter"
)

// The accounts, roles, and configuration the workloads above name.
const (
	// serviceAccountName is the account every workload built from the control
	// plane's own image runs as. Its name appears in the control plane's
	// SB_K8S_ADMIN_SERVICE_ACCOUNTS list, so it is not derived.
	serviceAccountName = "simplyblock-sa"

	// clusterRoleName and clusterRoleBindingName grant that account what the
	// control plane's Kubernetes-side work needs.
	clusterRoleName        = "simplyblock-role"
	clusterRoleBindingName = "simplyblock-binding"

	// serviceReaderRoleName and serviceReaderBindingName let the namespace's
	// default account resolve services and endpoints, which is how the control
	// plane's own tooling finds its peers.
	serviceReaderRoleName    = "simplyblock-service-reader"
	serviceReaderBindingName = "simplyblock-service-reader-binding"

	// configMapName holds the log level every workload reads through a
	// configMapKeyRef, so changing it reaches all of them at once.
	configMapName = "simplyblock-config"

	// objectStoreConfigName is the bucket configuration the object store's
	// consumers read.
	objectStoreConfigName = "simplyblock-objstore-config"

	// clusterFileConfigMapName is written by the FoundationDB operator rather
	// than by this one, and mounted by every workload that talks to the
	// database. The install neither creates nor reconciles it; it names it so
	// the mounts can.
	clusterFileConfigMapName = "simplyblock-fdb-cluster-config"

	// fdbOperatorServiceAccount and the roles beside it are the FoundationDB
	// operator's own, applied with it.
	fdbOperatorServiceAccount = "simplyblock-fdb-controller-manager"
	fdbOperatorRoleName       = "simplyblock-fdb-manager-role"
	fdbOperatorRoleBinding    = "simplyblock-fdb-manager-rolebinding"
	fdbOperatorClusterRole    = "simplyblock-fdb-manager-clusterrole"
	fdbOperatorClusterBinding = "simplyblock-fdb-manager-clusterrolebinding"

	// fdbPodServiceAccount is what the FoundationDB pods themselves run as. The
	// unified monitor in each pod writes locality annotations on its own pod to
	// signal the operator, which the namespace's default account cannot do.
	fdbPodServiceAccount = "simplyblock-fdb-cluster-pods"
	fdbPodRoleName       = "simplyblock-fdb-cluster-pods"
	fdbPodRoleBinding    = "simplyblock-fdb-cluster-pods"
)

// The ports the install publishes.
const (
	// webAPIPort is where the management API listens, and the port
	// status.endpoint resolves to.
	webAPIPort = 5000

	// minioAPIPort and minioConsolePort are the object store's two listeners.
	minioAPIPort     = 9000
	minioConsolePort = 9001

	// fdbExporterPort is where the exporter serves /metrics.
	fdbExporterPort = 9444
)

// The volumes and paths shared by every workload built from the control plane's
// image.
const (
	// clusterFileVolume mounts the FoundationDB cluster file at the path the
	// client library reads by default, so nothing has to set FDB_CLUSTER_FILE.
	clusterFileVolume = "fdb-cluster-file"
	clusterFilePath   = "/etc/foundationdb/fdb.cluster"
	clusterFileKey    = "cluster-file"
	clusterFileSubURL = "fdb.cluster"
)

// appLabel is the selector key every workload here is matched by. It is `app`
// rather than one of the recommended Kubernetes labels because that is what the
// running deployments carry, and a Deployment's selector is immutable.
const appLabel = "app"
