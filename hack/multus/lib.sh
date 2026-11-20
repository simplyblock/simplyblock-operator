# Settings and manifests shared by install.sh and uninstall.sh: the Multus
# thin plugin, the CNI reference plugins it needs, and the one-shot node
# cleanup. Sourced, not run. The defaults fit k3s running its own Flannel.
# With Flannel disabled (a cluster on Cilium), k3s stops pointing containerd at
# its own directories and uses /etc/cni/net.d and /opt/cni/bin, so set
# CNI_CONF_DIR and CNI_BIN_DIR to those, and for another distribution too.

MULTUS_VERSION=${MULTUS_VERSION:-v4.2.2}
PLUGINS_VERSION=${PLUGINS_VERSION:-1.6.2}
PLUGINS_ARCH=${PLUGINS_ARCH:-amd64}
CNI_CONF_DIR=${CNI_CONF_DIR:-/var/lib/rancher/k3s/agent/etc/cni/net.d}
CNI_BIN_DIR=${CNI_BIN_DIR:-/var/lib/rancher/k3s/data/cni}

# The plugins a data NIC needs. dhcp is left out: it needs a daemon the
# nodes do not run, and storage addresses have to be stable.
PLUGINS="macvlan ipvlan static host-device tuning vlan"

# Lists the plugin files this tooling added to CNI_BIN_DIR, so uninstall
# removes those and never a file that was there before.
MARKER=.dev-multus-added

# confirm names the cluster and stops unless --yes was given.
confirm() {
	echo "cluster: $(kubectl config current-context) ($(kubectl config view --minify -o 'jsonpath={.clusters[0].cluster.server}'))"
	echo "node paths: conf=${CNI_CONF_DIR} bin=${CNI_BIN_DIR}"
	[ "${ASSUME_YES:-}" = 1 ] || { echo "re-run with --yes to proceed"; exit 1; }
}

# multus_manifest prints the upstream Multus manifest with this tooling's
# paths, a pinned image, and cleanup of the generated config when the pod exits.
multus_manifest() {
	curl -fsSL "https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/${MULTUS_VERSION}/deployments/multus-daemonset.yml" |
		MULTUS_VERSION=${MULTUS_VERSION} CNI_CONF_DIR=${CNI_CONF_DIR} CNI_BIN_DIR=${CNI_BIN_DIR} python3 -I -c '
import os, sys
s = sys.stdin.read()
s = s.replace("multus-cni:snapshot", "multus-cni:" + os.environ["MULTUS_VERSION"])
s = s.replace("path: /etc/cni/net.d", "path: " + os.environ["CNI_CONF_DIR"])
s = s.replace("path: /opt/cni/bin", "path: " + os.environ["CNI_BIN_DIR"])
flag = "        - \"--cni-conf-dir=/host/etc/cni/net.d\"\n"
assert flag in s, "upstream manifest changed: no --cni-conf-dir argument to extend"
extra = "        - \"--cleanup-config-on-exit=true\"\n"
# The shim runs on the host, so the kubeconfig path it is given must be the host path.
extra += "        - \"--multus-kubeconfig-file-host=" + os.environ["CNI_CONF_DIR"] + "/multus.d/multus.kubeconfig\"\n"
s = s.replace(flag, flag + extra)
sys.stdout.write(s)
'
}

# plugins_manifest prints the DaemonSet that copies the reference plugins
# onto each node and records the ones it added.
plugins_manifest() {
	cat <<YAML
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: dev-cni-plugins
  namespace: kube-system
spec:
  selector:
    matchLabels: {name: dev-cni-plugins}
  template:
    metadata:
      labels: {name: dev-cni-plugins}
    spec:
      tolerations:
      - operator: Exists
      initContainers:
      - name: install
        image: curlimages/curl:8.10.1
        securityContext: {runAsUser: 0}
        command: ["sh", "-c"]
        args:
        - |
          set -eu
          cd /tmp
          curl -fsSL -o p.tgz https://github.com/containernetworking/plugins/releases/download/v${PLUGINS_VERSION}/cni-plugins-linux-${PLUGINS_ARCH}-v${PLUGINS_VERSION}.tgz
          for p in ${PLUGINS}; do
            tar -xzf p.tgz ./\$p
            [ -e /host/cni/\$p ] && continue
            cp \$p /host/cni/\$p
            echo \$p >> /host/cni/${MARKER}
          done
        volumeMounts:
        - {name: cnibin, mountPath: /host/cni}
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.9
      volumes:
      - name: cnibin
        hostPath: {path: ${CNI_BIN_DIR}}
YAML
}

# cleanup_manifest prints the DaemonSet that undoes the node-side changes:
# the generated Multus config and kubeconfig, the shim, and the plugins the
# marker lists. It reports ready once its node is clean.
cleanup_manifest() {
	cat <<YAML
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: dev-multus-cleanup
  namespace: kube-system
spec:
  selector:
    matchLabels: {name: dev-multus-cleanup}
  template:
    metadata:
      labels: {name: dev-multus-cleanup}
    spec:
      tolerations:
      - operator: Exists
      containers:
      - name: cleanup
        image: busybox:1.36
        securityContext: {privileged: true}
        command: ["sh", "-c"]
        args:
        - |
          set -u
          rm -f /host/conf/00-multus.conf
          rm -rf /host/conf/multus.d
          rm -f /host/bin/multus-shim
          if [ -f /host/bin/${MARKER} ]; then
            while read -r f; do rm -f "/host/bin/\$f"; done < /host/bin/${MARKER}
            rm -f /host/bin/${MARKER}
          fi
          touch /tmp/done
          echo cleaned
          while true; do sleep 3600; done
        readinessProbe:
          exec: {command: ["test", "-f", "/tmp/done"]}
          periodSeconds: 2
        volumeMounts:
        - {name: conf, mountPath: /host/conf}
        - {name: bin, mountPath: /host/bin}
      volumes:
      - name: conf
        hostPath: {path: ${CNI_CONF_DIR}}
      - name: bin
        hostPath: {path: ${CNI_BIN_DIR}}
YAML
}
