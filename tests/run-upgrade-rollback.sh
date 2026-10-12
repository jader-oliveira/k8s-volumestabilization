#!/usr/bin/env bash
# Upgrade / rollback safety test for the resize feature.
#
#   OLD operator + OLD CRD  ->  stabilize a volume
#   apply NEW CRD           ->  old operator must keep working (additive schema)
#   swap to NEW operator    ->  nothing is recreated or rewritten; status.currentSize appears
#   set spec.target.size    ->  volume grows
#   swap back to OLD        ->  rollback: old operator tolerates size + keeps volume Bound
#   swap to NEW again       ->  picks up where it left off
#
# Throw-away cluster ONLY (kind/minikube): it deletes and re-creates the operator and the CRD.
#
# Required env:
#   OLD_MANIFEST  path to the previous release's quickstart/operator.yaml (CRD + operator)
#   OLD_IMAGE     previous operator image (already loadable by the cluster)
#   NEW_IMAGE     new operator image under test
# Optional: KUBECTL, NS, SC, NEW_CRD (default crds/volumestabilization-crd.yaml)
set -uo pipefail

KUBECTL=${KUBECTL:-kubectl}
NS=${NS:-vs-upgrade-test}
SC=${SC:-nfs-synology}
OLD_MANIFEST=${OLD_MANIFEST:?set OLD_MANIFEST to the previous quickstart/operator.yaml}
OLD_IMAGE=${OLD_IMAGE:?set OLD_IMAGE}
NEW_IMAGE=${NEW_IMAGE:?set NEW_IMAGE}
HERE=$(cd "$(dirname "$0")" && pwd)
NEW_CRD=${NEW_CRD:-$HERE/../crds/volumestabilization-crd.yaml}
OP_NS=volume-stabilization
OP=volume-stabilization-operator
PASS=0

k() { $KUBECTL "$@"; }
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { PASS=$((PASS + 1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() {
  printf '  \033[31mFAIL\033[0m %s\n' "$*"
  k -n "$NS" get vs,pvc 2>&1 | sed 's/^/    /'
  k -n "$OP_NS" logs deploy/$OP --tail=20 2>&1 | cut -c1-200 | sed 's/^/    /'
  exit 1
}
wait_eq() { # desc want timeout kubectl-args...
  local d=$1 w=$2 t=$3 g="" i=0
  shift 3
  while [ $i -lt "$t" ]; do
    g=$(k "$@" 2>/dev/null || true)
    [ "$g" = "$w" ] && { ok "$d ($w)"; return 0; }
    sleep 1; i=$((i + 1))
  done
  fail "$d: wanted '$w', got '$g'"
}
assert_eq() { local d=$1 w=$2 g; shift 2; g=$(k "$@" 2>/dev/null || true); [ "$g" = "$w" ] && ok "$d ($w)" || fail "$d: wanted '$w', got '$g'"; }
set_image() {
  k -n "$OP_NS" set image deploy/$OP '*='"$1" >/dev/null
  k -n "$OP_NS" patch deploy $OP --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >/dev/null 2>&1
  k -n "$OP_NS" rollout status deploy/$OP --timeout=120s >/dev/null 2>&1 || fail "rollout of $1 failed"
  sleep 4
}
cleanup() {
  k -n "$NS" patch vs --all --type=merge -p '{"spec":{"deletionProtection":false}}' >/dev/null 2>&1
  k -n "$NS" delete vs --all --wait=false >/dev/null 2>&1; sleep 3
  k delete ns "$NS" --wait=false >/dev/null 2>&1
  k delete pv vs-upg-pv >/dev/null 2>&1
}
trap cleanup EXIT

say "Install OLD operator ($OLD_IMAGE) with its own CRD"
k delete vs --all -A --wait=false >/dev/null 2>&1
k delete crd volumestabilizations.synology.csi.io --wait=true --timeout=60s >/dev/null 2>&1
k delete ns $OP_NS --wait=true --timeout=60s >/dev/null 2>&1
k apply -f "$OLD_MANIFEST" >/dev/null || fail "apply old manifest"
set_image "$OLD_IMAGE"
ok "old operator running ($(k -n $OP_NS get deploy $OP -o 'jsonpath={.spec.template.spec.containers[0].image}'))"
k get crd volumestabilizations.synology.csi.io -o 'jsonpath={.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.target.properties}' | grep -q '"size"' \
  && fail "old CRD unexpectedly already has size" || ok "old CRD has no spec.target.size"

say "Stabilize a volume with the OLD operator"
k create ns "$NS" >/dev/null 2>&1 || true
k apply -f - >/dev/null <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: $SC}
provisioner: kubernetes.io/no-provisioner
allowVolumeExpansion: true
reclaimPolicy: Retain
---
apiVersion: v1
kind: PersistentVolume
metadata: {name: vs-upg-pv}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: $SC
  nfs: {server: 10.255.255.1, path: /vs-upg}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: seed, namespace: $NS}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC
  resources: {requests: {storage: 1Gi}}
---
apiVersion: synology.csi.io/v1
kind: VolumeStabilization
metadata: {name: vs, namespace: $NS}
spec:
  sourcePVC: {name: seed}
  target: {pvcName: data}
  deletionProtection: true
EOF
wait_eq "VS Bound" Bound 90 -n "$NS" get vs vs -o 'jsonpath={.status.phase}'
wait_eq "PVC Bound" Bound 60 -n "$NS" get pvc data -o 'jsonpath={.status.phase}'
UID_BEFORE=$(k -n "$NS" get pvc data -o 'jsonpath={.metadata.uid}')
PV_BEFORE=$(k -n "$NS" get pvc data -o 'jsonpath={.spec.volumeName}')
CLASS_BEFORE=$(k -n "$NS" get pvc data -o 'jsonpath={.spec.storageClassName}')

say "Apply the NEW CRD while the OLD operator is still running"
k apply --server-side --force-conflicts -f "$NEW_CRD" >/dev/null 2>&1 || fail "apply new CRD"
k get crd volumestabilizations.synology.csi.io -o 'jsonpath={.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.target.properties}' | grep -q '"size"' \
  && ok "new CRD installed (additive)" || fail "new CRD missing size"
sleep 8
assert_eq "old operator unaffected: VS still Bound" Bound -n "$NS" get vs vs -o 'jsonpath={.status.phase}'
assert_eq "PVC UID unchanged" "$UID_BEFORE" -n "$NS" get pvc data -o 'jsonpath={.metadata.uid}'
k -n "$NS" get vs vs -o wide | head -2 | sed 's/^/    /'

say "Swap to the NEW operator ($NEW_IMAGE)"
RV_BEFORE=$(k -n "$NS" get vs vs -o 'jsonpath={.metadata.resourceVersion}')
set_image "$NEW_IMAGE"
assert_eq "VS still Bound" Bound -n "$NS" get vs vs -o 'jsonpath={.status.phase}'
assert_eq "PVC NOT recreated (same UID)" "$UID_BEFORE" -n "$NS" get pvc data -o 'jsonpath={.metadata.uid}'
assert_eq "same PV" "$PV_BEFORE" -n "$NS" get pvc data -o 'jsonpath={.spec.volumeName}'
assert_eq "storageClassName untouched" "$CLASS_BEFORE" -n "$NS" get pvc data -o 'jsonpath={.spec.storageClassName}'
assert_eq "PVC request untouched" 1Gi -n "$NS" get pvc data -o 'jsonpath={.spec.resources.requests.storage}'
wait_eq "status.currentSize populated" 1Gi 30 -n "$NS" get vs vs -o 'jsonpath={.status.currentSize}'
RV1=$(k -n "$NS" get vs vs -o 'jsonpath={.metadata.resourceVersion}')
sleep 12
RV2=$(k -n "$NS" get vs vs -o 'jsonpath={.metadata.resourceVersion}')
[ "$RV1" = "$RV2" ] && ok "VS stable after upgrade (no loop)" || fail "VS rewritten after upgrade ($RV1 -> $RV2)"
[ -z "$(k -n "$NS" get vs vs -o 'jsonpath={.status.conditions[?(@.type=="ResizeRejected")].status}')" ] && ok "no ResizeRejected" || fail "unexpected ResizeRejected"
if k -n $OP_NS logs deploy/$OP --since=2m 2>/dev/null | grep -qiE "panic|level\":\"error|ERROR"; then
  k -n $OP_NS logs deploy/$OP --since=2m 2>/dev/null | grep -iE "panic|ERROR" | head -5 | cut -c1-200
  fail "errors in the new operator log"
fi
ok "no errors in the new operator log"

say "Resize with the NEW operator"
k -n "$NS" patch vs vs --type=merge -p '{"spec":{"target":{"size":"2Gi"}}}' >/dev/null
wait_eq "PVC request grown" 2Gi 30 -n "$NS" get pvc data -o 'jsonpath={.spec.resources.requests.storage}'
k patch pv "$PV_BEFORE" --type=merge -p '{"spec":{"capacity":{"storage":"2Gi"}}}' >/dev/null
k -n "$NS" patch pvc data --subresource=status --type=merge -p '{"status":{"capacity":{"storage":"2Gi"}}}' >/dev/null
wait_eq "Resized=True" True 60 -n "$NS" get vs vs -o 'jsonpath={.status.conditions[?(@.type=="Resized")].status}'

say "ROLLBACK to the OLD operator with size set and a Resized condition present"
set_image "$OLD_IMAGE"
assert_eq "old operator keeps VS Bound" Bound -n "$NS" get vs vs -o 'jsonpath={.status.phase}'
assert_eq "PVC UID unchanged" "$UID_BEFORE" -n "$NS" get pvc data -o 'jsonpath={.metadata.uid}'
assert_eq "PVC keeps the grown size" 2Gi -n "$NS" get pvc data -o 'jsonpath={.spec.resources.requests.storage}'
assert_eq "spec.target.size preserved" 2Gi -n "$NS" get vs vs -o 'jsonpath={.spec.target.size}'
RV1=$(k -n "$NS" get vs vs -o 'jsonpath={.metadata.resourceVersion}'); sleep 10
RV2=$(k -n "$NS" get vs vs -o 'jsonpath={.metadata.resourceVersion}')
[ "$RV1" = "$RV2" ] && ok "old operator not looping on the new fields" || fail "old operator rewriting VS ($RV1 -> $RV2)"

say "Roll FORWARD again"
set_image "$NEW_IMAGE"
assert_eq "VS Bound" Bound -n "$NS" get vs vs -o 'jsonpath={.status.phase}'
wait_eq "currentSize repopulated" 2Gi 30 -n "$NS" get vs vs -o 'jsonpath={.status.currentSize}'
wait_eq "Resized=True again" True 30 -n "$NS" get vs vs -o 'jsonpath={.status.conditions[?(@.type=="Resized")].status}'
assert_eq "PVC UID unchanged through the whole journey" "$UID_BEFORE" -n "$NS" get pvc data -o 'jsonpath={.metadata.uid}'

say "RESULT: $PASS checks passed"
