#!/usr/bin/env bash
# End-to-end test for declarative resize: spec.target.size on a VolumeStabilization.
#
# Covers (each step asserts, the script exits non-zero on the first failure):
#   1  stabilize a volume with no size set            -> PVC untouched, status.currentSize reported
#   2  CRD validation                                 -> "30", "-1Gi", "abc" rejected by the API server
#   3  grow via the CR                                -> PVC request grows, Resized False then True
#   4  idempotency / no reconcile loop                -> exactly one expansion, VS resourceVersion stable
#   5  shrink                                         -> ResizeRejected, PVC untouched, cleared when fixed
#   6  self-heal after PVC deletion                   -> recreated at the GROWN size on the SAME PV
#   7  second grow after a heal                       -> still works
#   8  StorageClass without allowVolumeExpansion      -> ExpansionRejected, reported once, no hot loop
#   9  size removed from the spec                     -> resize conditions cleared, PVC untouched
#
# MODES
#   EMULATE_CSI=true  (default) Uses a static NFS PV and plays the CSI resizer itself by patching
#                     PV capacity / PVC status. For a throw-away kind/minikube cluster.
#   EMULATE_CSI=false Uses a real dynamically provisioning StorageClass (SC=...) and waits for the
#                     real external-resizer. Creates small PVCs only, in an isolated namespace.
#
# REQUIRES the operator (v0.1.9+) already running and the updated CRD installed.
#
# Env: KUBECTL (default kubectl)  NS  SC  NOEXPAND_SC  START  GROW  GROW2  TIMEOUT  KEEP_NAMESPACE
set -uo pipefail

KUBECTL=${KUBECTL:-kubectl}
NS=${NS:-vs-resize-test}
SC=${SC:-nfs-synology}
NOEXPAND_SC=${NOEXPAND_SC:-nfs-noexpand-test}
START=${START:-1Gi}
GROW=${GROW:-2Gi}
GROW2=${GROW2:-3Gi}
TIMEOUT=${TIMEOUT:-90}
EMULATE_CSI=${EMULATE_CSI:-true}
KEEP_NAMESPACE=${KEEP_NAMESPACE:-false}
OP_NS=${OP_NS:-volume-stabilization}
OP_DEPLOY=${OP_DEPLOY:-volume-stabilization-operator}

PASS=0
k() { $KUBECTL "$@"; }
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { PASS=$((PASS + 1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() {
  printf '  \033[31mFAIL\033[0m %s\n' "$*"
  echo "--- VS:"; k -n "$NS" get vs -o wide 2>&1 | sed 's/^/    /'
  echo "--- PVC:"; k -n "$NS" get pvc 2>&1 | sed 's/^/    /'
  echo "--- operator log (tail):"; k -n "$OP_NS" logs deploy/"$OP_DEPLOY" --tail=25 2>&1 | cut -c1-220 | sed 's/^/    /'
  exit 1
}

# wait_eq "<description>" "<expected>" <timeout> <kubectl args that print one value ...>
wait_eq() {
  local desc=$1 want=$2 to=$3
  shift 3
  local got="" i=0
  while [ $i -lt "$to" ]; do
    got=$(k "$@" 2>/dev/null || true)
    [ "$got" = "$want" ] && { ok "$desc ($want)"; return 0; }
    sleep 1
    i=$((i + 1))
  done
  fail "$desc: wanted '$want', got '$got' after ${to}s"
}
# assert_eq "<description>" "<expected>" <kubectl args ...>
assert_eq() {
  local desc=$1 want=$2
  shift 2
  local got
  got=$(k "$@" 2>/dev/null || true)
  [ "$got" = "$want" ] && ok "$desc ($want)" || fail "$desc: wanted '$want', got '$got'"
}
vs_field() { k -n "$NS" get vs "$1" -o "jsonpath=$2"; }
cond_status() { k -n "$NS" get vs "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].status}"; }
cond_reason() { k -n "$NS" get vs "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].reason}"; }
pvc_req() { k -n "$NS" get pvc "$1" -o 'jsonpath={.spec.resources.requests.storage}'; }
pvc_cap() { k -n "$NS" get pvc "$1" -o 'jsonpath={.status.capacity.storage}'; }
op_log_count() { k -n "$OP_NS" logs deploy/"$OP_DEPLOY" --since=10m 2>/dev/null | grep -c "$1" || true; }
set_size() { k -n "$NS" patch vs "$1" --type=merge -p "{\"spec\":{\"target\":{\"size\":$2}}}" >/dev/null; }

# Plays the CSI resizer in EMULATE_CSI mode; in real mode just waits for the driver.
finish_expansion() { # <pvc> <pv> <size>
  if [ "$EMULATE_CSI" = "true" ]; then
    k patch pv "$2" --type=merge -p "{\"spec\":{\"capacity\":{\"storage\":\"$3\"}}}" >/dev/null
    k -n "$NS" patch pvc "$1" --subresource=status --type=merge -p "{\"status\":{\"capacity\":{\"storage\":\"$3\"}}}" >/dev/null
  fi
}

cleanup() {
  [ "$KEEP_NAMESPACE" = "true" ] && { echo "KEEP_NAMESPACE=true: leaving $NS"; return; }
  k -n "$NS" patch vs --all --type=merge -p '{"spec":{"deletionProtection":false}}' >/dev/null 2>&1
  k -n "$NS" delete vs --all --wait=false >/dev/null 2>&1
  sleep 3
  k delete ns "$NS" --wait=false >/dev/null 2>&1
  if [ "$EMULATE_CSI" = "true" ]; then
    k delete pv vs-resize-pv-main vs-resize-pv-noexp >/dev/null 2>&1
    k delete sc "$NOEXPAND_SC" >/dev/null 2>&1
  fi
}
trap cleanup EXIT

# ------------------------------------------------------------------ setup --
say "Setup (EMULATE_CSI=$EMULATE_CSI, namespace=$NS)"
k get crd volumestabilizations.synology.csi.io >/dev/null 2>&1 || fail "CRD not installed"
k get crd volumestabilizations.synology.csi.io -o 'jsonpath={.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.target.properties.size.type}' | grep -q string \
  || fail "installed CRD has no spec.target.size — apply the v0.1.9 CRD first"
ok "CRD with spec.target.size installed"
k -n "$OP_NS" rollout status deploy/"$OP_DEPLOY" --timeout=60s >/dev/null 2>&1 || fail "operator not running"
ok "operator running ($(k -n "$OP_NS" get deploy "$OP_DEPLOY" -o 'jsonpath={.spec.template.spec.containers[0].image}'))"
k create ns "$NS" >/dev/null 2>&1 || true

if [ "$EMULATE_CSI" = "true" ]; then
  k apply -f - >/dev/null <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: $SC}
provisioner: kubernetes.io/no-provisioner
allowVolumeExpansion: true
reclaimPolicy: Retain
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: $NOEXPAND_SC}
provisioner: kubernetes.io/no-provisioner
allowVolumeExpansion: false
reclaimPolicy: Retain
---
apiVersion: v1
kind: PersistentVolume
metadata: {name: vs-resize-pv-main}
spec:
  capacity: {storage: $START}
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: $SC
  nfs: {server: 10.255.255.1, path: /vs-resize-main}
---
apiVersion: v1
kind: PersistentVolume
metadata: {name: vs-resize-pv-noexp}
spec:
  capacity: {storage: $START}
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: $NOEXPAND_SC
  nfs: {server: 10.255.255.1, path: /vs-resize-noexp}
EOF
  PV_MAIN=vs-resize-pv-main
  PV_NOEXP=vs-resize-pv-noexp
fi

mkpvc() { # <name> <class>
  k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1, namespace: $NS}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $2
  resources: {requests: {storage: $START}}
EOF
}
mkvs() { # <name> <source> <target> [size-json-or-empty]
  local size=""
  [ -n "${4:-}" ] && size=", \"size\": $4"
  k apply -f - >/dev/null <<EOF
apiVersion: synology.csi.io/v1
kind: VolumeStabilization
metadata: {name: $1, namespace: $NS}
spec:
  sourcePVC: {name: $2}
  target: {pvcName: $3 $size}
  deletionProtection: false
EOF
}

# ------------------------------------------------- 1. stabilize, no size --
say "1. Stabilize a volume with no size configured"
mkpvc seed-main "$SC"
wait_eq "seed PVC bound" Bound 60 -n "$NS" get pvc seed-main -o 'jsonpath={.status.phase}'
[ "$EMULATE_CSI" = "true" ] || PV_MAIN=$(k -n "$NS" get pvc seed-main -o 'jsonpath={.spec.volumeName}')
mkvs vs-main seed-main data-main
wait_eq "VS reaches Bound" Bound "$TIMEOUT" -n "$NS" get vs vs-main -o 'jsonpath={.status.phase}'
wait_eq "stable PVC bound" Bound 60 -n "$NS" get pvc data-main -o 'jsonpath={.status.phase}'
assert_eq "stable PVC on the SAME PV" "$PV_MAIN" -n "$NS" get pvc data-main -o 'jsonpath={.spec.volumeName}'
assert_eq "PVC request untouched" "$START" -n "$NS" get pvc data-main -o 'jsonpath={.spec.resources.requests.storage}'
wait_eq "status.currentSize reported" "$START" 30 -n "$NS" get vs vs-main -o 'jsonpath={.status.currentSize}'
[ -z "$(cond_status vs-main Resized)" ] && ok "no Resized condition while size is unmanaged" || fail "unexpected Resized condition"

# -------------------------------------------------- 2. CRD validation ----
say "2. CRD validation (API server)"
for bad in '"30"' '"-1Gi"' '"abc"' '"10 Gi"'; do
  if set_size vs-main "$bad" 2>/dev/null; then
    fail "API server accepted size=$bad"
  else
    ok "size=$bad rejected"
  fi
done
assert_eq "spec.target.size still unset" "" -n "$NS" get vs vs-main -o 'jsonpath={.spec.target.size}'

# ------------------------------------------------------- 3. grow --------
say "3. Grow $START -> $GROW via the CR"
set_size vs-main "\"$GROW\""
wait_eq "PVC request grown" "$GROW" 30 -n "$NS" get pvc data-main -o 'jsonpath={.spec.resources.requests.storage}'
wait_eq "Resized=False while the driver works" False 30 -n "$NS" get vs vs-main -o 'jsonpath={.status.conditions[?(@.type=="Resized")].status}'
assert_eq "currentSize still the REAL size" "$START" -n "$NS" get vs vs-main -o 'jsonpath={.status.currentSize}'
finish_expansion data-main "$PV_MAIN" "$GROW"
wait_eq "Resized=True after the driver finishes" True "$TIMEOUT" -n "$NS" get vs vs-main -o 'jsonpath={.status.conditions[?(@.type=="Resized")].status}'
wait_eq "currentSize follows capacity" "$GROW" 30 -n "$NS" get vs vs-main -o 'jsonpath={.status.currentSize}'
k -n "$NS" get events --field-selector involvedObject.name=vs-main 2>/dev/null | grep -q Resizing && ok "Resizing event emitted" || fail "no Resizing event"

# ------------------------------------------- 4. idempotency / no loop ----
say "4. Idempotency: one expansion, no reconcile storm"
RV1=$(vs_field vs-main '{.metadata.resourceVersion}')
sleep 12
RV2=$(vs_field vs-main '{.metadata.resourceVersion}')
[ "$RV1" = "$RV2" ] && ok "VS resourceVersion stable for 12s ($RV1)" || fail "VS is being rewritten ($RV1 -> $RV2): reconcile loop"
N=$(k -n "$OP_NS" logs deploy/"$OP_DEPLOY" --since=10m 2>/dev/null | grep -c "Expanding stable PVC.*data-main") || N=0
[ "$N" -eq 1 ] && ok "exactly one expansion logged" || fail "expansion logged $N times"

# ----------------------------------------------------------- 5. shrink --
say "5. Shrink is rejected"
set_size vs-main "\"$START\""
wait_eq "ResizeRejected=True" True 30 -n "$NS" get vs vs-main -o 'jsonpath={.status.conditions[?(@.type=="ResizeRejected")].status}'
assert_eq "reason" ShrinkNotSupported -n "$NS" get vs vs-main -o 'jsonpath={.status.conditions[?(@.type=="ResizeRejected")].reason}'
assert_eq "PVC request NOT lowered" "$GROW" -n "$NS" get pvc data-main -o 'jsonpath={.spec.resources.requests.storage}'
set_size vs-main "\"$GROW\""
wait_eq "ResizeRejected cleared once spec is valid again" "" 30 -n "$NS" get vs vs-main -o 'jsonpath={.status.conditions[?(@.type=="ResizeRejected")].status}'

# --------------------------------------------------------- 6. self-heal --
say "6. Self-heal keeps the grown size"
k -n "$NS" delete pvc data-main --wait=false >/dev/null
# keep the CSI emulation honest: the PV capacity has already been raised in step 3.
wait_eq "PVC recreated and bound" Bound "$TIMEOUT" -n "$NS" get pvc data-main -o 'jsonpath={.status.phase}'
assert_eq "recreated on the SAME PV" "$PV_MAIN" -n "$NS" get pvc data-main -o 'jsonpath={.spec.volumeName}'
assert_eq "recreated at the GROWN size (not the original $START)" "$GROW" -n "$NS" get pvc data-main -o 'jsonpath={.spec.resources.requests.storage}'
wait_eq "VS back to Bound" Bound 30 -n "$NS" get vs vs-main -o 'jsonpath={.status.phase}'

# ------------------------------------------------------ 7. grow again ---
say "7. Grow again after a heal: $GROW -> $GROW2"
set_size vs-main "\"$GROW2\""
wait_eq "PVC request grown" "$GROW2" 30 -n "$NS" get pvc data-main -o 'jsonpath={.spec.resources.requests.storage}'
finish_expansion data-main "$PV_MAIN" "$GROW2"
wait_eq "Resized=True" True "$TIMEOUT" -n "$NS" get vs vs-main -o 'jsonpath={.status.conditions[?(@.type=="Resized")].status}'

# ------------------------------- 8. StorageClass that cannot expand ------
say "8. StorageClass without allowVolumeExpansion"
NOEXP_CLASS=$NOEXPAND_SC
if [ "$EMULATE_CSI" != "true" ]; then
  NOEXP_CLASS=${NOEXPAND_SC}
  k get sc "$NOEXP_CLASS" >/dev/null 2>&1 || { echo "  (real mode: set NOEXPAND_SC to a class with allowVolumeExpansion=false, or this step is skipped)"; NOEXP_CLASS=""; }
fi
if [ -n "$NOEXP_CLASS" ]; then
  mkpvc seed-noexp "$NOEXP_CLASS"
  wait_eq "seed PVC bound" Bound 60 -n "$NS" get pvc seed-noexp -o 'jsonpath={.status.phase}'
  mkvs vs-noexp seed-noexp data-noexp "\"$GROW\""
  wait_eq "VS Bound" Bound "$TIMEOUT" -n "$NS" get vs vs-noexp -o 'jsonpath={.status.phase}'
  wait_eq "ResizeRejected=True" True 30 -n "$NS" get vs vs-noexp -o 'jsonpath={.status.conditions[?(@.type=="ResizeRejected")].status}'
  assert_eq "reason" ExpansionRejected -n "$NS" get vs vs-noexp -o 'jsonpath={.status.conditions[?(@.type=="ResizeRejected")].reason}'
  echo "    message: $(vs_field vs-noexp '{.status.conditions[?(@.type=="ResizeRejected")].message}' | cut -c1-160)"
  assert_eq "PVC untouched" "$START" -n "$NS" get pvc data-noexp -o 'jsonpath={.spec.resources.requests.storage}'
  RV1=$(vs_field vs-noexp '{.metadata.resourceVersion}')
  sleep 12
  RV2=$(vs_field vs-noexp '{.metadata.resourceVersion}')
  [ "$RV1" = "$RV2" ] && ok "no hot loop on a rejected expansion" || fail "VS rewritten ($RV1 -> $RV2)"
fi

# ------------------------------------------------- 9. size removed ------
say "9. Remove spec.target.size"
k -n "$NS" patch vs vs-main --type=json -p '[{"op":"remove","path":"/spec/target/size"}]' >/dev/null
wait_eq "Resized condition cleared" "" 30 -n "$NS" get vs vs-main -o 'jsonpath={.status.conditions[?(@.type=="Resized")].status}'
assert_eq "PVC keeps its size" "$GROW2" -n "$NS" get pvc data-main -o 'jsonpath={.spec.resources.requests.storage}'

say "RESULT: $PASS checks passed"
