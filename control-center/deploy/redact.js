// njs filters for the control plane upstream (/controlplane/) and the log
// store (/graylog/api/, logbody).
//
// The simplyblock control plane API v2 returns credentials in some records --
// a cluster's `secret` is the bearer token that authorizes that cluster's whole
// API. The console's proxy authenticates as the console, so anything it passes
// on reaches every browser that can open the console. This filter makes that
// impossible: the body is parsed, every credential-shaped field is replaced,
// and a body that is not JSON is never passed through at all (fail closed).
//
// Loaded by docker-entrypoint.sh only when the njs module is present; without
// it the control plane upstream is not enabled.

const SECRET_KEY = /^(secret|password|passwd|token|access_key|secret_key|private_key|api_key|.*[_-]secret|.*[_-]password|.*[_-]token|.*[_-]key_secret)$/i;
const REDACTED = "[redacted]";

function scrub(v) {
  if (Array.isArray(v)) return v.map(scrub);
  if (v !== null && typeof v === "object") {
    const o = {};
    Object.keys(v).forEach(function (k) {
      o[k] = SECRET_KEY.test(k) ? (v[k] === null || v[k] === "" ? v[k] : REDACTED) : scrub(v[k]);
    });
    return o;
  }
  return v;
}

// The body changes length, so the upstream's Content-Length must go.
function headers(r) {
  delete r.headersOut["Content-Length"];
  r.headersOut["Content-Type"] = "application/json";
}

function notJson(status) {
  return JSON.stringify({kind: "Status", apiVersion: "v1", status: "Failure", code: status || 502,
    reason: "BadGateway", message: "the control plane answered with a body that is not JSON (HTTP " + status + ")"});
}

// Buffers the whole response, then sends it once, scrubbed. Control plane
// responses are lists of records, not streams; the console never asks for the
// watch variant on this upstream. njs runs every request in its own VM, so
// this module-level buffer belongs to the one response being filtered.
let pending = "";
function body(r, data, flags) {
  pending += data;
  if (!flags.last) return;
  let out = "";
  if (pending.length) {
    try { out = JSON.stringify(scrub(JSON.parse(pending))); }
    catch (e) { out = notJson(r.status); }
  }
  pending = "";
  r.sendBuffer(out, flags);
}

// Log lines are free text: a credential printed into one ("password=...",
// "Authorization: Bearer ...", '"secret": "..."') is masked wherever it
// appears, on top of the field-name rule above.
const TEXT_SECRET = /((?:secret|password|passwd|token|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key)["']?\s*[:=]\s*["']?)([^\s"',;}\]]+)/gi;
const AUTH_HEADER = /\b(Bearer|Basic)\s+[A-Za-z0-9._~+\/=-]{8,}/g;
function scrubText(t) {
  return t.replace(TEXT_SECRET, "$1" + REDACTED).replace(AUTH_HEADER, "$1 " + REDACTED);
}
function scrubLog(v) {
  if (typeof v === "string") return scrubText(v);
  if (Array.isArray(v)) return v.map(scrubLog);
  if (v !== null && typeof v === "object") {
    const o = {};
    Object.keys(v).forEach(function (k) {
      o[k] = SECRET_KEY.test(k) ? (v[k] === null || v[k] === "" ? v[k] : REDACTED) : scrubLog(v[k]);
    });
    return o;
  }
  return v;
}
function notJsonLog(status) {
  return JSON.stringify({kind: "Status", apiVersion: "v1", status: "Failure", code: status || 502,
    reason: "BadGateway", message: "the log store answered with a body that is not JSON (HTTP " + status + ")"});
}
// Same buffering as body(): one search page is one JSON document.
let pendingLog = "";
function logbody(r, data, flags) {
  pendingLog += data;
  if (!flags.last) return;
  let out = "";
  if (pendingLog.length) {
    try { out = JSON.stringify(scrubLog(JSON.parse(pendingLog))); }
    catch (e) { out = notJsonLog(r.status); }
  }
  pendingLog = "";
  r.sendBuffer(out, flags);
}

export default {headers, body, logbody, scrub, scrubLog, scrubText, SECRET_KEY};
