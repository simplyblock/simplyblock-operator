// How a support bundle is collected: the one command a support agent hands a
// customer, shown wherever the tool waits for a bundle. It lives with the
// server because the page reads it from the API, and the CLI banner reads it
// from here, so the two never disagree.

package server

// CollectCommand runs the bundle script from simplyblock's webspace with its
// defaults: the namespace of the current kubectl context, and the newest
// discovery run in it.
const CollectCommand = "curl -s -L https://install.simplyblock.io/scripts/discovery-bundler | bash"

// CollectCommandWithOptions is the same, naming the namespace and the run.
const CollectCommandWithOptions = "curl -s -L https://install.simplyblock.io/scripts/discovery-bundler | bash -s -- -n <namespace> --run <run>"

// ManualStep is one command of collecting a bundle by hand, for a customer who
// cannot or will not run the script.
type ManualStep struct {
	Title   string `json:"title"`
	Command string `json:"command"`
}

// ManualCollection is the script's work as individual commands, in order. They
// share shell variables, so they run in one shell session, and together they
// write the same archive the script does.
func ManualCollection() []ManualStep {
	return []ManualStep{
		{"Name the discovery run and its namespace",
			`NS=<namespace>; RUN=<run>`},
		{"Create the bundle directory",
			`D=simplyblock-bundle-$RUN-$(date -u +%Y%m%dT%H%M%SZ); mkdir -p "$D/reports" "$D/logs/probe" "$D/logs/operator"`},
		{"Record the kubectl and API server versions",
			`kubectl version > "$D/version.txt" 2>&1`},
		{"Collect the discovery run",
			`kubectl get operatorops "$RUN" -n "$NS" -o yaml > "$D/operatorops.yaml"; ` +
				`kubectl describe operatorops "$RUN" -n "$NS" > "$D/operatorops-describe.txt"`},
		{"Collect the namespace's events and the nodes",
			`kubectl get events -n "$NS" -o wide --sort-by=.lastTimestamp > "$D/events.txt"; ` +
				`kubectl get nodes -o yaml > "$D/nodes.yaml"; kubectl get nodes -o wide > "$D/nodes-wide.txt"`},
		{"Collect the probe reports (a run name over 63 characters is cut to 63 in the label)",
			`SEL="storage.simplyblock.io/nodeprobe-run=$(printf %s "${RUN:0:63}" | sed -E 's/[._-]+$//')"; ` +
				`kubectl get configmaps -n "$NS" -l "$SEL" -o yaml > "$D/probe-reports.yaml"; ` +
				`for cm in $(kubectl get configmaps -n "$NS" -l "$SEL" -o name); do ` +
				`kubectl get "$cm" -n "$NS" -o 'jsonpath={.data.report\.json}' > "$D/reports/${cm#configmap/}.json"; done`},
		{"Collect the probe pods and their logs (kept for an hour after the run)",
			`kubectl get jobs,pods -n "$NS" -l "$SEL" -o wide > "$D/probe-pods.txt"; ` +
				`for pod in $(kubectl get pods -n "$NS" -l "$SEL" -o name); do ` +
				`kubectl logs "$pod" -n "$NS" --all-containers --timestamps > "$D/logs/probe/${pod#pod/}.log"; done`},
		{"Collect the ClusterDeploymentConfig the run wrote",
			`CDC=$(kubectl get operatorops "$RUN" -n "$NS" -o jsonpath='{.status.configRef}'); ` +
				`if [ -n "$CDC" ]; then kubectl get clusterdeploymentconfigs "$CDC" -n "$NS" -o yaml > "$D/cdc.yaml"; ` +
				`kubectl describe clusterdeploymentconfigs "$CDC" -n "$NS" > "$D/cdc-describe.txt"; fi`},
		{"Collect the deployed control plane, clusters, and nodes",
			`kubectl get controlplanes,storageclusters,storagenodes -n "$NS" -o yaml > "$D/deployment.yaml"`},
		{"Collect the operator's log",
			`OPNS=$(kubectl get pods -A -l control-plane=simplyblock-operator -o jsonpath='{.items[0].metadata.namespace}'); ` +
				`kubectl logs -n "$OPNS" -l control-plane=simplyblock-operator --all-containers --timestamps --tail=-1 ` +
				`> "$D/logs/operator/simplyblock-operator.log"`},
		{"Pack the bundle",
			`touch "$D/errors.txt"; COPYFILE_DISABLE=1 tar -czf "$D.tar.gz" "$D" && echo "wrote $D.tar.gz"`},
	}
}
