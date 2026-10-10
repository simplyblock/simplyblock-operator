// Development configuration: the preview runs against the in-browser fixture
// backend. In a container this file is REPLACED at startup by
// docker-entrypoint.sh from the pod's environment, and mock is false.
window.SB_CONFIG = {
  k8sBase: "/k8s",
  operatorBase: "/operator/v1",
  helmBase: "/helm/v1",
  promBase: "/prometheus/api/v1",
  agentBase: "/operator/v1/agent",
  // the log store (Graylog search API) behind the console's proxy
  graylogBase: "/graylog/api",
  namespace: "simplyblock",
  // "full": the storage control plane console with a DR section.
  // "dr":   the DR-only console for a hub without a simplyblock control plane
  //         (only dr.simplyblock.io is read; no operator API, no storage views).
  mode: "full",
  // Ramen's ops namespace on the DR hub: default namespace of discovered
  // ProtectedApplications and of the access review.
  drNamespace: "ramen-ops",
  // dr-hub's own namespace on the hub: the discovery graph's data shards
  // (ConfigMaps dr-graph-<site>-<n>) live there.
  drHubNamespace: "dr-simplyblock",
  // The brand mark. In a pod this is the vendored copy — the CSP is
  // img-src 'self' and there is no egress to the public internet.
  logoUrl: "https://simplyblock.io/assets/images/Logo-white.svg",
  // "serviceaccount": the nginx sidecar in front of this UI attaches the pod's
  // ServiceAccount token, and the browser never sees a credential.
  // "passthrough": the browser sends this token itself.
  authMode: "serviceaccount",
  token: "dev",
  mock: true
};
