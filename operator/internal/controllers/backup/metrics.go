// The time series the data-protection band exports.
//
// They are declared apart from the controllers that fill them for the reason
// every metrics file in this repository is: a collector is process-global state
// registered once at init, while a reconciler is constructed per manager and may
// be constructed more than once in a test.
//
// One of them is the one to alert on, and it is the only one here that measures
// a promise rather than an activity. A policy that stopped running produces no
// failure and no event: it simply stops producing backups, and the only thing
// that notices is backupAgeSeconds climbing past the schedule.
//
// design-storagebackup.md §11.2 is the specification, and
// design-crd-model.md §7.12 is the naming rule the six suffixes come from.

package backup

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// backupSizeBuckets spans a megabyte to a terabyte in powers of four, which is
// the range a volume's copies actually fall in. Linear buckets would put every
// backup this product takes in one of them.
var backupSizeBuckets = prometheus.ExponentialBuckets(1<<20, 4, 11)

// backupDurationBuckets span a second to roughly a day. A backup is incremental
// after the first one, so most complete in seconds while the first copy of a
// large volume runs for hours, and one set has to hold both.
var backupDurationBuckets = prometheus.ExponentialBuckets(1, 4, 9)

var (
	backupCompletionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_storagebackup_completions_total",
			Help: "Backups that reached a terminal phase, by result.",
		},
		[]string{"cluster", "policy", "result"},
	)

	// The copy's own reported start-to-finish rather than transitions the
	// operator observed. The stream coalesces, so a small backup can be
	// Available before the operator ever saw it start, and timing the phases it
	// happened to observe would report zero for the fastest backups.
	backupDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagebackup_duration_seconds",
			Help:    "How long a backup took, as the backup itself reported it.",
			Buckets: backupDurationBuckets,
		},
		[]string{"cluster", "policy"},
	)

	backupSizeBytes = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagebackup_size_bytes",
			Help:    "Size of the backups taken, which is what a bill tracks.",
			Buckets: backupSizeBuckets,
		},
		[]string{"cluster", "policy"},
	)

	// The one to alert on: a policy that silently stopped running shows up here
	// and nowhere else.
	backupAgeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagebackup_age_seconds",
			Help: "Age of the newest restorable backup of the claim.",
		},
		[]string{"cluster", "claim"},
	)

	backupAvailableCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagebackup_available_count",
			Help: "Restorable backups the claim has.",
		},
		[]string{"cluster", "claim"},
	)

	policyAttachedClaimsCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagebackuppolicy_attached_claims_count",
			Help: "Claims the backup policy currently covers.",
		},
		[]string{"cluster", "policy"},
	)

	backupOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_storagebackup_operations_total",
			Help: "Backup operations that reached a terminal phase, by action and result.",
		},
		[]string{"cluster", "action", "result"},
	)

	// Everything else here is about taking copies. This is the only number that
	// says what getting one back costs, which makes it the recovery time
	// objective, measured.
	backupOperationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagebackup_operation_duration_seconds",
			Help:    "How long a backup operation took from acquiring its lock to a terminal phase.",
			Buckets: backupDurationBuckets,
		},
		[]string{"cluster", "action"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		backupCompletionsTotal,
		backupDurationSeconds,
		backupSizeBytes,
		backupAgeSeconds,
		backupAvailableCount,
		policyAttachedClaimsCount,
		backupOperationsTotal,
		backupOperationDurationSeconds,
	)
}
