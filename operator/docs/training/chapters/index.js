// The chapters of the training, in the order they are taught, grouped into the
// parts the index page shows. Every page loads the files listed here, so adding
// a chapter is adding its file and one line below. The number a chapter shows
// is its position across all parts, and its results are kept under its id, so
// moving a chapter changes its number and nothing else.
const CHAPTER_PARTS = [
  { title: 'Deploying a cluster', files: [
    '01-openshift-compact.js',
    '02-talos.js',
    '03-eks.js',
    '04-gke.js',
    '05-openshift-sixteen-nodes.js',
    '06-openshift-placement-groups.js',
    '07-failure-domains.js',
  ] },
  { title: 'Edge sites', files: [
    '08-two-node-edge.js',
    '09-edge-management-cluster.js',
  ] },
  { title: 'Growing and changing a cluster', files: [
    '10-cluster-expansion.js',
    '11-node-removal.js',
    '12-node-host-migration.js',
  ] },
  { title: 'Moving data without downtime', files: [
    '13-volume-migration-nodes.js',
    '14-volume-migration-clusters.js',
    '15-cluster-live-migration.js',
  ] },
  { title: 'Protecting data', files: [
    '16-backup-restore.js',
    '17-async-dr.js',
    '18-async-dr-arbiter.js',
  ] },
  { title: 'Security', files: [
    '19-dhchap.js',
    '20-external-kms.js',
  ] },
  { title: 'Services', files: [
    '21-s3-buckets-ha.js',
  ] },
];
const CHAPTER_FILES = CHAPTER_PARTS.flatMap(p => p.files);
