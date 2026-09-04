// Package prometheus reads the telemetry simplyblock exports about itself.
//
// It exists as its own package rather than as part of [controlplane] because
// the two speak to different endpoints over different protocols with different
// failure modes: the control plane answers its v2 REST API over its own base
// URL and bearer token, while these numbers are PromQL results from whichever
// Prometheus scrapes the deployment, and an absent or unscraped Prometheus is a
// missing sample rather than a failed request.
//
// # What is here
//
// One [Provider] per Prometheus endpoint, exposing the metric families both
// consumers have reason to read:
//
//   - Node write latency, at a chosen percentile, from the rebalancer's probe
//     ([Provider.ClusterLatencies]).
//   - Per-volume IOPS and throughput ([Provider.VolumeIO]).
//   - Per-volume and per-device capacity samples
//     ([Provider.VolumeCapacity], [Provider.DeviceCapacity]).
//
// # Why capacity is read here and not from the control-plane API
//
// The control plane's own DTOs carry a capacity block, and for a logical volume
// it reports zeros: the numbers exist only in the metrics the same service
// exports. A device's DTO is populated, but its watch stream never pushes an
// update when capacity moves, so a cached copy is only as fresh as the last
// snapshot. Either way this is the source that has the current answer.
//
// # Freshness
//
// Every value is a scrape, so it is at most one scrape interval old and there
// is no way to ask for anything newer. Prometheus offers no push or watch for
// samples, so a caller that wants a current number queries when it needs one
// rather than keeping a cache this package would have to invalidate.
package prometheus
