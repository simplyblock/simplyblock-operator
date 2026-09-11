package main

import (
	"net/http"
	"sort"
	"strings"
)

// AuthZ answers the authorization queries the console's read/write path makes
// (multi-cluster RBAC design §5): SelfSubjectRulesReview per namespace,
// SubjectAccessReview spot checks, and a scope-projection listing the scope
// tree the caller may see.
//
// IMPORTANT stand-in (see PARADIGM.md): the mock has no OIDC/impersonation, so
// it cannot evaluate real RBAC. It answers from a configured "viewer" identity
// instead. The default viewer is a global admin (matches the console's
// serviceaccount auth mode, where the pod token is cluster-admin); narrow it
// with --viewer to exercise the console's disabled/hidden states. And, as the
// design itself insists (§5.3), these are DISPLAY APIs — the mock does not
// enforce them on the CRUD path.
type AuthZ struct {
	store  *Store
	viewer Viewer
}

// Viewer is the impersonation stand-in. Global grants everything (the default).
// Otherwise Grants lists (role, namespace) pairs; namespace "" is cluster scope.
type Viewer struct {
	Username string
	Groups   []string
	Global   bool
	Grants   []ViewerGrant
}

type ViewerGrant struct {
	Role      string // an sb:* role name
	Namespace string // "" = cluster scope
}

// parseViewer builds a Viewer from the --viewer flag:
//
//	global                         (default) everything allowed
//	none                           nothing allowed (renders the 403 states)
//	<role>@<namespace>[,<role>@<ns>...]   e.g. sb:cluster-admin@sb-sc-cluster1
func parseViewer(spec string) Viewer {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "global" {
		return Viewer{Username: "admin@simplyblock.io", Groups: []string{"sb-infra-admins"}, Global: true}
	}
	if spec == "none" {
		return Viewer{Username: "nobody@simplyblock.io"}
	}
	v := Viewer{Username: "viewer@simplyblock.io"}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		role, ns, _ := strings.Cut(part, "@")
		v.Grants = append(v.Grants, ViewerGrant{Role: strings.TrimSpace(role), Namespace: strings.TrimSpace(ns)})
	}
	return v
}

// allow reports whether the viewer may verb the resource in namespace.
func (a *AuthZ) allow(namespace, verb, resource string) bool {
	if a.viewer.Global {
		return true
	}
	write := verb != "get" && verb != "list" && verb != "watch"
	for _, gr := range a.viewer.Grants {
		if gr.Namespace != "" && gr.Namespace != namespace {
			continue
		}
		role, ok := sbRoleByName(gr.Role)
		if !ok {
			continue
		}
		if write && !role.Write {
			continue
		}
		for _, res := range role.Resources {
			if res == resource {
				return true
			}
		}
	}
	return false
}

// rulesFor returns the resourceRules the viewer holds in a namespace.
func (a *AuthZ) rulesFor(namespace string) []any {
	if a.viewer.Global {
		return []any{map[string]any{
			"verbs": adminVerbs(), "apiGroups": []any{sbGroup, ""}, "resources": []any{"*"},
		}}
	}
	var rules []any
	for _, gr := range a.viewer.Grants {
		if gr.Namespace != "" && gr.Namespace != namespace {
			continue
		}
		role, ok := sbRoleByName(gr.Role)
		if !ok {
			continue
		}
		verbs := readVerbs()
		if role.Write {
			verbs = adminVerbs()
		}
		resources := make([]any, len(role.Resources))
		for i, r := range role.Resources {
			resources[i] = r
		}
		rules = append(rules, map[string]any{
			"verbs": verbs, "apiGroups": []any{sbGroup}, "resources": resources,
		})
	}
	return rules
}

// ServeReview handles the authorization.k8s.io review kinds (create-only,
// non-persisted). Called from the K8s API handler when it sees that group.
func (a *AuthZ) ServeReview(w http.ResponseWriter, r *http.Request, resource string) {
	body, _ := readBody(r)
	spec, _ := body["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	switch resource {
	case "selfsubjectrulesreviews":
		ns, _ := spec["namespace"].(string)
		writeJSON(w, 201, map[string]any{
			"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectRulesReview",
			"status": map[string]any{
				"resourceRules":    a.rulesFor(ns),
				"nonResourceRules": []any{},
				"incomplete":       false,
			},
		})
	case "subjectaccessreviews", "selfsubjectaccessreviews", "localsubjectaccessreviews":
		ra, _ := spec["resourceAttributes"].(map[string]any)
		ns, verb, res := "", "get", ""
		if ra != nil {
			ns, _ = ra["namespace"].(string)
			verb, _ = ra["verb"].(string)
			res, _ = ra["resource"].(string)
		}
		allowed := a.allow(ns, verb, res)
		kind := map[string]string{
			"subjectaccessreviews":      "SubjectAccessReview",
			"selfsubjectaccessreviews":  "SelfSubjectAccessReview",
			"localsubjectaccessreviews": "LocalSubjectAccessReview",
		}[resource]
		writeJSON(w, 201, map[string]any{
			"apiVersion": "authorization.k8s.io/v1", "kind": kind,
			"status": map[string]any{
				"allowed": allowed,
				"reason":  ternStr(allowed, "granted by viewer", "no matching grant for the configured viewer"),
			},
		})
	default:
		writeJSON(w, 404, map[string]any{"kind": "Status", "status": "Failure", "reason": "NotFound", "code": 404,
			"message": "unknown authorization review " + resource})
	}
}

// Scopes serves the scope-projection (§5.2): the scope tree the viewer may
// see, enumerated from namespace hierarchy labels + a per-scope allow check.
func (a *AuthZ) Scopes(w http.ResponseWriter, _ *http.Request) {
	nsDef, _ := a.store.Lookup("", "v1", "namespaces")
	all, _ := a.store.List(nsDef, "", "", "")
	var scopes []any
	for _, ns := range all {
		labels, _ := getPath(ns, "metadata.labels").(map[string]any)
		if labels == nil {
			continue
		}
		kind, _ := labels[labScopeKind].(string)
		if kind == "" || kind == "system" {
			continue
		}
		name := getStr(ns, "metadata.name")
		// A scope is visible if the viewer can list something in it.
		visible := a.viewer.Global || len(a.rulesFor(name)) > 0
		if !visible {
			continue
		}
		scope := map[string]any{
			"id":        name,
			"kind":      kind,
			"namespace": name,
		}
		for _, k := range []string{labTenant, labManagedCluster, labStorageCluster} {
			if v, ok := labels[k].(string); ok {
				scope[strings.TrimPrefix(k, "simplyblock.io/")] = v
			}
		}
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		return scopes[i].(map[string]any)["id"].(string) < scopes[j].(map[string]any)["id"].(string)
	})
	writeJSON(w, 200, map[string]any{"status": "success", "data": scopes})
}

// Self serves an aggregated self-rules view (§5.3): the console fetches this
// once and evaluates button state locally.
func (a *AuthZ) Self(w http.ResponseWriter, _ *http.Request) {
	nsDef, _ := a.store.Lookup("", "v1", "namespaces")
	all, _ := a.store.List(nsDef, "", "", "")
	perNs := map[string]any{}
	for _, ns := range all {
		name := getStr(ns, "metadata.name")
		if rules := a.rulesFor(name); len(rules) > 0 {
			perNs[name] = rules
		}
	}
	writeJSON(w, 200, map[string]any{
		"status": "success",
		"data": map[string]any{
			"username":   a.viewer.Username,
			"groups":     a.viewer.Groups,
			"global":     a.viewer.Global,
			"namespaces": perNs,
			"incomplete": false,
		},
	})
}
