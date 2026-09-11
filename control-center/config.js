// Development configuration: the preview runs against the in-browser fixture
// backend. In a container this file is REPLACED at startup by
// docker-entrypoint.sh from the pod's environment, and mock is false.
window.SB_CONFIG = {
  k8sBase: "/k8s",
  operatorBase: "/operator/v1",
  helmBase: "/helm/v1",
  promBase: "/prometheus/api/v1",
  agentBase: "/operator/v1/agent",
  namespace: "simplyblock",
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
