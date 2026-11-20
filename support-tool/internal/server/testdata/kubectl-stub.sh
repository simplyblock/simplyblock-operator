#!/usr/bin/env bash
#
# A stub kubectl for the manual collection test: it answers each command the
# manual steps run from the fixture bundle directory in $FIXTURE, so the steps
# can be run as written and their archive read back.
F="$FIXTURE"
case "$*" in
  version*) echo "Client Version: v1.33.0"; echo "Server Version: v1.35.4" ;;
  "get operatorops "*"-o yaml"*) cat "$F/operatorops.yaml" ;;
  "get operatorops "*"configRef"*) echo -n discovered-discover-fleet-02 ;;
  "describe "*) echo "described" ;;
  "get events "*) cat "$F/events.txt" ;;
  "get nodes -o yaml"*) cat "$F/nodes.yaml" ;;
  "get nodes -o wide"*) echo "nodes" ;;
  "get configmaps "*"-o yaml"*) echo "kind: List" ;;
  "get pods -n "*) ;;
  "get configmaps "*"-o name"*) for f in "$F"/reports/*.json; do echo "configmap/$(basename "$f" .json)"; done ;;
  "get configmap/"*) name="${2#configmap/}"; cat "$F/reports/$name.json" ;;
  "get clusterdeploymentconfigs "*) cat "$F/cdc.yaml" ;;
  "get controlplanes,storageclusters,storagenodes "*) cat "$F/deployment.yaml" ;;
  "get jobs,pods "*) echo "No resources found" ;;
  "get pods -A "*) echo -n simplyblock ;;
  logs*) echo "operator log" ;;
  *) echo "stub kubectl: unexpected $*" >&2; exit 1 ;;
esac
