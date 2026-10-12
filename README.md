# Volume Stabilization Operator

> A Kubernetes operator that gives dynamically-provisioned NFS volumes a **stable, predictable PVC name** — without recreating the volume or touching data on the NAS.

[![Go](https://img.shields.io/badge/Go-1.21-blue)](https://golang.org)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.27%2B-blue)](https://kubernetes.io)
[![controller-runtime](https://img.shields.io/badge/controller--runtime-v0.17-blue)](https://github.com/kubernetes-sigs/controller-runtime)
[![Image](https://img.shields.io/badge/image-jaderoliver%2Fvolume--stabilization--operator%3Av0.1.9-blue)](https://hub.docker.com/r/jaderoliver/volume-stabilization-operator)
[![License](https://img.shields.io/badge/License-MIT-green)](LICENSE)

---

## Table of Contents

- [The Problem](#the-problem)
- [The Solution](#the-solution)
- [Architecture](#architecture)
- [How It Works](#how-it-works)
- [Key Design Decisions](#key-design-decisions)
- [CRD Reference](#crd-reference)
- [Quickstart](#quickstart)
- [Day-2 Operations](#day-2-operations)
  - [Resize a volume](#resize-a-volume)
  - [Deleting the stable PVC](#deleting-the-stable-pvc-accidental-or-intentional)
  - [Deletion protection](#deletion-protection)
  - [Removing everything cleanly](#removing-everything-cleanly)
- [Compatibility](#compatibility)
- [Controller Internals (Code Map)](#controller-internals-code-map)
- [CI/CD Pipeline](#cicd-pipeline)
- [Project Structure](#project-structure)

---

## The Problem

When Kubernetes provisions a volume dynamically through a CSI driver (e.g. Synology NFS CSI), it generates a PVC and PV with a UUID-based name:

```
pvc-3892bba3-75e2-4a59-99c6-07b3652a98b9
```

This name is **non-deterministic**. It changes every time the PVC is recreated. This creates a fundamental conflict with declarative GitOps workflows and Helm charts:

- A `Deployment` or `StatefulSet` manifest hard-codes `claimName: my-app-data`. If that PVC is deleted and recreated dynamically, a **new PV is provisioned** (new UUID, new NAS folder) and all data from the old volume is orphaned.
- Helm `existingClaim` patterns require the PVC to exist before install, but the name is only known after first provisioning.
- Disaster recovery on a new cluster requires manually reconstructing the PVC/PV binding for every volume.

### Why not just delete and recreate the PV?

With the Synology CSI driver, **deleting a PV object causes the CSI driver to remove the NFS export on the NAS**, even when the reclaim policy is `Retain`. The data becomes inaccessible (`mount: access denied by server`) the moment the PV object is garbage-collected. This rules out any approach that involves PV deletion.

---

## The Solution

The Volume Stabilization Operator introduces a `VolumeStabilization` custom resource that performs a **non-destructive rename** of a dynamically-provisioned PVC:

```
pvc/my-app-data-dynamic  (UUID-named, CSI-provisioned)
        │
        ▼  VolumeStabilization CR
pvc/my-app-data          (stable, predictable name)
```

The original PV — and therefore the NFS export on the NAS — is **never deleted or recreated**. Only the PVC object is replaced. The NAS folder, all its data, and all mount permissions remain intact.

Once stabilized, the operator continuously **watches and self-heals**: if the stable PVC is ever deleted (accidental `kubectl delete`, namespace wipe, Helm uninstall), the operator recreates it within seconds, re-binding to the exact same PV and data.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│  Kubernetes Cluster                                             │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  volume-stabilization namespace                          │   │
│  │                                                          │   │
│  │  ┌────────────────────────────────────────────────────┐  │   │
│  │  │  volume-stabilization-operator (Deployment)        │  │   │
│  │  │                                                    │  │   │
│  │  │  ┌──────────────────────────────────────────────┐  │  │   │
│  │  │  │  VolumeStabilizationReconciler               │  │  │   │
│  │  │  │                                              │  │  │   │
│  │  │  │  Watches: VolumeStabilization CRs            │  │  │   │
│  │  │  │  Watches: PersistentVolumeClaims (by annot.) │  │  │   │
│  │  │  └──────────────────────────────────────────────┘  │  │   │
│  │  └────────────────────────────────────────────────────┘  │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  application namespace (e.g. default)                    │   │
│  │                                                          │   │
│  │  VolumeStabilization CR ──────────────────────────────►  │   │
│  │    spec.sourcePVC: my-app-data-dynamic                   │   │
│  │    spec.target.pvcName: my-app-data                      │   │
│  │                                                          │   │
│  │  PVC: my-app-data-dynamic (deleted after stabilization)  │   │
│  │  PVC: my-app-data ◄── statically bound ─────────────►    │   │
│  │  PV:  pvc-3892bba3-...  (original, never deleted)        │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  Synology NAS                                            │   │
│  │  /volume1/k8s-csi-pvc-3892bba3-...  (never touched)      │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

### Component Overview

| Component | Kind | Namespace | Purpose |
|---|---|---|---|
| `VolumeStabilization` | CRD | any | Declares source → target PVC rename |
| `volume-stabilization-operator` | Deployment | `volume-stabilization` | Runs the reconciliation loop |
| `volume-stabilization-operator` | ServiceAccount + ClusterRole | cluster | RBAC for PV/PVC/VS access |

---

## How It Works

The operator implements a deterministic state machine. Each reconciliation loop advances the `VolumeStabilization` CR through these phases:

```
                ┌─────────┐
                │ Pending │  CR created; source PVC found
                └────┬────┘
                     │
             ┌───────▼──────────┐
             │   Discovering    │  Waiting for source PVC to be Bound
             └───────┬──────────┘
                     │
             ┌───────▼──────────┐
             │   Capturing      │  Reads PV spec; stores server, path,
             │                  │  capacity, accessModes, mountOptions
             └───────┬──────────┘
                     │
        ┌────────────▼─────────────┐
        │    DeletingOriginal      │  1. Deletes source PVC only (NOT PV)
        │                          │  2. Waits for PV → Released
        │                          │  3. Patches PV: clears claimRef +
        │                          │     storageClassName → Available
        └────────────┬─────────────┘
                     │
             ┌───────▼──────────┐
             │    Creating      │  Creates new PVC with:
             │                  │    volumeName: <original-pv-uuid>
             │                  │    storageClassName: ""  (static bind)
             └───────┬──────────┘
                     │
               ┌─────▼──────┐
               │   Bound ✓  │  Self-healing active
               └─────┬──────┘
                     │
              PVC deleted? ──► back to Creating (same PV, same data)
```

### Reconciliation Detail: DeletingOriginal Phase

This is the most critical phase. The naive approach of deleting the PV causes data loss with Synology CSI. Instead:

1. **Delete only the PVC** — the PV transitions from `Bound` → `Released`
2. **Wait** for the PV to reach `Released` or `Available`
3. **Patch the PV** in-place: set `spec.claimRef = nil` and preserve the original
  `spec.storageClassName` by default
4. The PV transitions to `Available` — ready to be statically bound

No NFS export is touched. No NAS API call is made. The folder at `/volume1/k8s-csi-pvc-<uuid>` is untouched.

### Reconciliation Detail: Creating Phase

Creates a new PVC that statically pre-binds to the original PV:

```yaml
spec:
  volumeName: pvc-3892bba3-75e2-4a59-99c6-07b3652a98b9  # original PV
  storageClassName: nfs-synology                         # preserved by default for resize support
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 1Gi
```

Kubernetes binds this PVC to the PV immediately — no new provisioning occurs because `volumeName`
pre-binds the claim to the existing PV.

### Self-Healing (Bound Phase)

The operator watches **all PVC events** and maps them back to their owning VS CR via the annotation `synology.csi.io/stabilization-cr`. When a stabilized PVC is deleted:

1. Watch fires → `reconcileBound()` runs
2. PVC not found → patch original PV to clear stale `claimRef`
3. Drop back to `Creating` → PVC recreated in seconds

The workload pod (if still running) may briefly lose the mount and recover, or stay running if the deletion was brief enough. A pod in `Pending` will start normally once the PVC exists.

---

## Key Design Decisions

### 1. Never delete the original PV

The Synology CSI driver (`csi.san.synology.com`) calls the NAS API to **remove the NFS export** when a PV object is deleted from Kubernetes, regardless of `reclaimPolicy: Retain`. This was validated in live testing — the mount returns `access denied by server` immediately after PV deletion. The operator therefore never issues a PV delete.

### 2. Preserve StorageClass for expansion compatibility

The operator preserves the original StorageClass on the stabilized PVC and reused PV by default.
That keeps the claim eligible for normal PVC expansion when the StorageClass supports it.

If you explicitly set `spec.target.storageClassName: ""`, the operator falls back to fully static
binding. That still binds through `spec.volumeName`, but Kubernetes will treat the stabilized PVC as
non-dynamically-provisioned for resize purposes.

### 3. No OwnerReferences on PV or PVC

PVs are cluster-scoped; a namespaced CR cannot own a cluster-scoped object — Kubernetes rejects such OwnerReferences. PVCs intentionally have no OwnerReference so they **survive VS CR deletion** (data safety). The relationship is tracked via annotations (`synology.csi.io/stabilization-cr`) and VS status fields.

### 4. Two-phase deployment pattern

The source PVC must be created and bound **before** the VS CR is applied, and the workload **after** stabilization completes. This is the necessary ordering — it cannot be parallelized. This maps cleanly to Helm pre-install hooks and GitOps wave ordering.

### 5. Deletion protection via finalizer

The operator always attaches a finalizer (`synology.csi.io/volumestabilization-finalizer`) to every VS CR. When `spec.deletionProtection: true` is set and `kubectl delete` is run, Kubernetes sets `DeletionTimestamp` but cannot garbage-collect the object while the finalizer exists. The controller detects this, records a persistent `DeletionBlocked` status condition, and continues normal phase reconciliation (including stable PVC self-healing) while the VS is in `Terminating` state. The finalizer is only removed — and the VS actually deleted — once `spec.deletionProtection: false` is set and `DeletionTimestamp` is still set.

### 5. PVC watch instead of Owns()

`Owns()` sets OwnerReferences — prohibited here. Instead, `SetupWithManager` registers a `Watches()` on PVCs with a custom mapper function that reads the annotation to find the parent VS CR. This gives event-driven healing without cross-scope ownership.

---

## CRD Reference

### `VolumeStabilization`

```yaml
apiVersion: synology.csi.io/v1
kind: VolumeStabilization
metadata:
  name: stabilize-my-app-data
  namespace: default
spec:
  sourcePVC:
    name: my-app-data-dynamic    # required: PVC to rename (must be Bound to a CSI NFS PV)
    namespace: default           # optional: defaults to CR namespace

  target:
    pvcName: my-app-data         # required: stable PVC name to create
    namespace: default           # optional: defaults to source namespace
    storageClassName: nfs-synology # optional: leave unset to preserve the original StorageClass
                                   # Set to "" only when you explicitly want static non-resizable binding
    size: 30Gi                     # optional: desired capacity. Change it and apply to grow the volume
                                   # (grow-only, unit required). See "Resize a volume".

  preserveAnnotations: true      # optional: copy annotations to new PVC (default: true)
  preserveLabels: true           # optional: copy labels to new PVC (default: true)
  waitForDeletion: true          # optional: wait for full PVC deletion (default: true)

  deletionProtection: true       # optional: block accidental kubectl delete (default: false)
                                 # While true, any delete on this CR is blocked until you
                                 # explicitly patch it to false first.
```

### Status Fields

```yaml
status:
  phase: Bound                   # Pending | Discovering | Capturing | DeletingOriginal
                                 # | Creating | Bound | Failed
  message: "Volume stabilized: PV=pvc-3892..., PVC=default/my-app-data"
  currentSize: 30Gi              # actual PVC capacity right now (compare with spec.target.size)

  originalPV:                    # Captured from source PV — persisted for self-healing
    name: pvc-3892bba3-75e2-...
    server: 192.168.0.200
    path: /volume1/k8s-csi-pvc-3892bba3-...
    storage: 1Gi
    accessModes: [ReadWriteMany]
    mountOptions: [nfsvers=4.1, hard, noatime]

  newPV:
    name: pvc-3892bba3-75e2-...  # same as originalPV.name (PV is reused, not recreated)
  newPVC:
    name: my-app-data
    namespace: default

  conditions:
    - type: SourcePVCExists
    - type: SourcePVCBound
    - type: CapturePV
    - type: DeleteOriginal
    - type: CreateNew
    - type: Stabilized          # True = fully operational and self-healing active
    - type: Resized             # True = PVC capacity has reached spec.target.size
                                # False (reason Resizing) = expansion in progress
    - type: ResizeRejected      # True = spec.target.size cannot be honoured; reason is one of
                                # ShrinkNotSupported | InvalidSize | ExpansionRejected
    - type: DeletionBlocked     # True = kubectl delete was attempted but blocked by
                                #        deletionProtection=true; clears on actual deletion
```

### Short names

```bash
kubectl get vs          # VolumeStabilization
kubectl get volstab     # alias
```

---

## Quickstart

### Prerequisites

- Kubernetes 1.27+ cluster
- `kubectl` configured
- A `StorageClass` backed by an NFS CSI driver (e.g. Synology NFS CSI, nfs-subdir-external-provisioner)

### 1. Install the operator

```bash
kubectl apply -f operator.yaml

kubectl rollout status deployment/volume-stabilization-operator \
  -n volume-stabilization --timeout=90s
```

Verify:

```bash
kubectl get pods -n volume-stabilization
# NAME                                             READY   STATUS    AGE
# volume-stabilization-operator-7d9f8b6c4-xk2p9   1/1     Running   30s
```

### 2. Create the initial dynamic PVC

This represents what a Helm chart or your application provisioner does on first install. Only needed once per volume.

```bash
kubectl apply -f example-usage.yaml --selector step=dynamic-pvc

kubectl wait --for=jsonpath='{.status.phase}'=Bound \
  pvc/my-app-data-dynamic -n default --timeout=120s
```

### 3. Apply the VolumeStabilization CR

```bash
kubectl apply -f example-usage.yaml --selector step=stabilize

kubectl wait --for=jsonpath='{.status.phase}'=Bound \
  volumestabilization/stabilize-my-app-data -n default --timeout=120s
```

Check the result:

```bash
kubectl get vs,pvc,pv -n default

# NAME                                                    SOURCE PVC          TARGET PVC    PHASE
# volumestabilization.synology.csi.io/stabilize-my-app   my-app-data-dynamic my-app-data   Bound
#
# NAME                       STATUS  VOLUME                                    CAPACITY
# persistentvolumeclaim/my-app-data  Bound  pvc-3892bba3-75e2-4a59-99c6-...   1Gi
```

`my-app-data-dynamic` is gone. `my-app-data` is bound to the **same PV and same NAS folder**.

### 4. Deploy your workload

```bash
kubectl apply -f example-usage.yaml --selector step=workload
# or your own manifest — no --selector needed in real projects
```

In your own `Deployment` / `StatefulSet`:

```yaml
volumes:
  - name: data
    persistentVolumeClaim:
      claimName: my-app-data   # stable — always exists while VS CR is present
```

---

## Day-2 Operations

### Resize a volume

Set `spec.target.size` on the `VolumeStabilization`, apply, done — independent of the application:

```yaml
spec:
  target:
    pvcName: my-app-data
    size: 30Gi          # was 15Gi
```

```bash
kubectl apply -f volumestabilization.yaml
kubectl get vs -n default
# NAME    SOURCE PVC   TARGET PVC    PHASE   SIZE   CURRENT   AGE
# my-app  ...          my-app-data   Bound   30Gi   15Gi      40d   ← CSI driver still expanding
# my-app  ...          my-app-data   Bound   30Gi   30Gi      40d   ← done
```

| Field / condition | Meaning |
|---|---|
| `spec.target.size` | Desired capacity. A unit is required (`30Gi`, not `30` — a bare number would mean bytes and is rejected by the CRD). Leave unset to not manage the size. |
| `status.currentSize` | The **actual** capacity (PVC `status.capacity`). Not just what was requested. |
| `Resized=False` / `Resizing` | PVC request raised; the CSI driver has not finished yet. |
| `Resized=True` | Capacity has reached `spec.target.size`. |
| `ResizeRejected=True` | `ShrinkNotSupported` (size below current), `InvalidSize` (≤ 0), or `ExpansionRejected` (e.g. StorageClass without `allowVolumeExpansion`; the API error is in the message). |

Rules worth knowing:

- **Grow-only.** Kubernetes cannot shrink a PVC; a smaller value is reported, never applied.
- **The size must be reachable.** The StorageClass must have `allowVolumeExpansion: true` and the claim must be `Bound`. A volume stabilized by an operator version ≤ v0.1.7 (storage class forced to `""`) cannot be expanded until its PV and PVC get a StorageClass back.
- **A PVC is created at the PV's real capacity, never above it.** A claim that requests more than its PV can never bind, and a volume can only be expanded after it is bound, so growth is always a second step. This also means self-healing recreates the PVC at the already-grown size.
- **Keep the application in sync.** If the application also tracks a size (for example Zalando's `volume.size`), set it to the same value so the two do not disagree.
- **Do not resize by editing the seed PVC.** That PVC is deleted after stabilization; re-applying it creates a new, unrelated volume.

Imperative alternative (works without this field, same result): `kubectl patch pvc my-app-data --type=merge -p '{"spec":{"resources":{"requests":{"storage":"30Gi"}}}}'` — but it is not in Git and a later `spec.target.size` of a smaller value will report `ShrinkNotSupported`.

### Deleting the stable PVC (accidental or intentional)

The operator detects the deletion within the watch event cycle (~1–5 seconds) and recreates the PVC bound to the same PV. No new NAS folder is created.

```bash
kubectl delete pvc my-app-data -n default
# ... 5 seconds later ...
kubectl get pvc my-app-data -n default
# NAME          STATUS  VOLUME                                     CAPACITY
# my-app-data   Bound   pvc-3892bba3-75e2-4a59-99c6-07b3652a98b9  1Gi   ← same PV
```

### Deleting the Deployment and PVC together

```bash
kubectl delete deployment my-app -n default
kubectl delete pvc my-app-data -n default
# Operator heals the PVC automatically.
kubectl apply -f my-app-deployment.yaml
# Pod starts immediately — PVC already exists.
```

### Full operation matrix

| Scenario | Operator response | Data safe? |
|---|---|---|
| Stable PVC deleted | Recreates PVC on same PV within ~5 s | ✅ |
| Deployment deleted, PVC deleted | Recreates PVC; re-apply workload normally | ✅ |
| Deployment deleted, PVC survives | No action needed; re-apply workload | ✅ |
| VS CR deleted | PVC and PV are **retained**; operator stops managing | ✅ |
| VS CR `kubectl delete` with `deletionProtection: true` | Blocked — VS stays `Terminating`; self-healing still active; `DeletionBlocked` condition recorded | ✅ |
| Source PVC re-created after stabilization | Ignored by the operator — **but the CSI driver still provisions a brand-new, unused volume (and NAS folder) for it.** Never re-apply the seed PVC manifest after stabilization. | ✅ (original), ⚠ orphan volume |
| `spec.target.size` raised | Stable PVC grown to that size by the CSI resizer; `Resized` condition tracks it | ✅ |
| `spec.target.size` lowered | Rejected (`ResizeRejected=ShrinkNotSupported`), PVC untouched | ✅ |
| Stable PVC deleted after a resize | Recreated on the same PV at the **grown** size | ✅ |
| Node where pod runs goes offline | Standard Kubernetes pod rescheduling; PVC already exists | ✅ |

### Inspecting status

```bash
# Full status
kubectl describe vs stabilize-my-app-data -n default

# Phase only
kubectl get vs stabilize-my-app-data -n default \
  -o jsonpath='{.status.phase}{"\n"}'

# Operator logs
kubectl logs -n volume-stabilization \
  deployment/volume-stabilization-operator --follow
```

### Resize recovery after disk full

When the stabilized PVC keeps the original StorageClass, normal Kubernetes expansion keeps working.
That means a workload can crashloop on `Quota exceeded` or `No space left on device`, the user can
resize the stabilized PVC (set `spec.target.size`, see [Resize a volume](#resize-a-volume)), and the
next restart can come up healthy on the larger filesystem.

An end-to-end rerun script is included at `tests/run-fill-expand-recover.sh`.

Example:

```bash
bash tests/run-fill-expand-recover.sh

# optional overrides
NAMESPACE=vs-fill-expand-test STORAGE_CLASS=nfs-synology TARGET_SIZE=2Gi \
  bash tests/run-fill-expand-recover.sh
```

The script performs this sequence automatically:

1. Creates a dynamic PVC and stabilizes it
2. Fills the stabilized PVC until the write fails
3. Starts a Deployment that crashloops on a startup write
4. Resizes the stabilized PVC
5. Waits for the Deployment to recover
6. Scales the Deployment to 2 replicas and verifies both pods start

### Deletion protection

Enable `spec.deletionProtection: true` to prevent accidental deletion of the VS CR (e.g. when running `kubectl delete -f` on a folder that includes your VS manifest).

While protection is active:
- Any `kubectl delete vs` is **blocked** — the CR remains `Terminating` but is not removed
- A persistent `DeletionBlocked` condition is written to status (visible in `kubectl describe`)
- A `DeletionProtected` warning event is emitted once
- Self-healing (stable PVC recreation) **continues to work** normally

```bash
# Check whether deletion is blocked
kubectl describe vs stabilize-my-app-data -n default
# Look for: Conditions: DeletionBlocked=True

# Lift protection and allow deletion
kubectl patch vs stabilize-my-app-data -n default \
  --type=merge -p '{"spec":{"deletionProtection":false}}'
# The VS is now deleted automatically (DeletionTimestamp was already set)
```

### Removing everything cleanly

```bash
# Delete workload first
kubectl delete deployment my-app -n default

# If deletionProtection=true, lift it first
kubectl patch vs stabilize-my-app-data -n default \
  --type=merge -p '{"spec":{"deletionProtection":false}}'

# Delete VS CR — PVC and PV are retained (data safe)
kubectl delete vs stabilize-my-app-data -n default

# Now manually delete PVC and PV if you want to deprovision storage
kubectl delete pvc my-app-data -n default
kubectl delete pv pvc-3892bba3-75e2-4a59-99c6-07b3652a98b9

# Uninstall operator
kubectl delete -f operator.yaml
```

---

## Compatibility

| Component | Version |
|---|---|
| Kubernetes | 1.27+ |
| Go | 1.21 |
| controller-runtime | v0.17 |
| k8s.io/api | v0.29.0 |
| Synology NFS CSI driver | `csi.san.synology.com` (any) |
| Plain NFS volumes | `spec.nfs` (any) |
| Architectures | `linux/amd64`, `linux/arm64` |

The operator detects two PV layouts automatically:

- **Native NFS** (`spec.nfs`) — reads `server` and `path` directly
- **Synology CSI** (`spec.csi`, driver `csi.san.synology.com`, `protocol=nfs`) — reads `server` from `volumeAttributes["dsm"]` and `path` from `volumeAttributes["baseDir"]`

---

## Controller Internals (Code Map)

Fast-orientation reference to the reconciliation logic for anyone (human or agent) returning to this
codebase. Everything below lives in
[operator/controllers/volumestabilization_controller.go](operator/controllers/volumestabilization_controller.go)
(~735 lines, single file — no sub-packages).

### Well-known constants, labels and annotations

| Symbol / Key | Value | Where it's used |
|---|---|---|
| `volumeStabilizationFinalizer` | `synology.csi.io/volumestabilization-finalizer` | Always attached to every VS CR; blocks GC while `deletionProtection: true` and the CR is `Terminating` |
| `stabilizedLabel` | `synology.csi.io/stabilized` | Set to `"true"` on the new stable PVC; also checked on the **source** PVC in `reconcilePending` to detect "already stabilized" (permanent `Failed`) |
| `originalPVCNameAnnotation` | `synology.csi.io/original-pvc-name` | Recorded on the new PVC for traceability back to the deleted dynamic PVC |
| `originalPVNameAnnotation` | `synology.csi.io/original-pv-name` | Recorded on the new PVC (same as `status.newPV.name`) |
| *(inline)* `synology.csi.io/managed-by` | `volume-stabilization-operator` | Label on the new PVC |
| *(inline)* `synology.csi.io/stabilized-at` | RFC3339 timestamp | Annotation on the new PVC, set at creation time |
| *(inline)* `synology.csi.io/stabilization-cr` | VS CR name | **The critical wiring**: written on the new PVC; read back by the `pvcMapper` in `SetupWithManager` to route PVC-deleted events to the right VS CR reconcile — this is what replaces `Owns()` |

### `Reconcile()` — top-level control flow (~line 63)

1. Fetch the VS CR (`r.Get`); `NotFound` → no-op (already deleted).
2. **Deletion branch** — `!vs.DeletionTimestamp.IsZero()`:
   - `deletionProtection: true` → if the `DeletionBlocked` condition isn't already `True`, set it
     + emit a `DeletionProtected` warning event once, then call `updateStatus` (which re-triggers
     reconcile). If already recorded, **fall through** to normal phase dispatch — this is what keeps
     `reconcileBound` self-healing active while the CR sits in `Terminating`.
   - `deletionProtection: false` → `reconcileDelete` (strip finalizer, retain PV/PVC, return).
3. Add the finalizer if missing (unconditionally — independent of `deletionProtection`), then
   `Requeue: true`.
4. **Stuck-`Failed` recovery**: if `phase == Failed` and the `AlreadyStabilized` reason is recorded,
   stop permanently (`return nil`); for any *other* failure reason, reset `phase = Pending` and retry
   — this is what makes "VS applied before the source PVC exists" self-heal instead of dead-ending
   (fixed in v0.1.4, see git log `04f872c`).
5. `switch vs.Status.Phase` → dispatch to exactly one `reconcile<Phase>` per call.

### Phase functions — one state, one function

| Function | Line | Transition | Responsibility / key details |
|---|---|---|---|
| `reconcilePending` | ~159 | `Pending → Discovering` | Looks up the source PVC. `NotFound` → **transient** `Failed` + 10s requeue (self-heals once the PVC shows up). Already labeled `stabilized=true` → **permanent** `Failed` with `AlreadyStabilized` reason (the one terminal failure that does NOT auto-retry) |
| `reconcileDiscovering` | ~207 | `Discovering → Capturing` | Polls every 5s until `sourcePVC.status.phase == Bound` |
| `reconcileCapturing` | ~247 | `Capturing → DeletingOriginal` | Reads the bound PV; **layout detection** switch: `pv.Spec.NFS != nil` (native NFS) vs. `pv.Spec.CSI.Driver == "csi.san.synology.com" && volumeAttributes["protocol"]=="nfs"` (reads `dsm`/`baseDir` from `volumeAttributes`); anything else → permanent `Failed` (`NotNFS`). Captures `status.originalPV` (server, path, capacity via `pv.Spec.Capacity.Storage()`, accessModes, **storageClassName**, reclaimPolicy, mountOptions — with sane fallback defaults if the PV omits them) |
| `reconcileDeletingOriginal` | ~360 | `DeletingOriginal → Creating` | **The data-safety-critical phase** — see breakdown below |
| `reconcileCreating` | ~442 | `Creating → Bound` | Re-checks the original PV is `Available` (2s requeue loop if not); idempotency check (`Get` target PVC name — if it exists, jump straight to `Bound`, e.g. after a controller restart mid-phase); builds labels/annotations from captured info (source PVC is already gone by this point); maps `status.originalPV.accessModes` strings back to `corev1.PersistentVolumeAccessMode`; constructs the PVC with `spec.volumeName = originalPVName` (the static pre-bind) and `spec.storageClassName = resolveTargetStorageClassName(vs)` |
| `reconcileBound` | ~575 | `Bound → Bound` (steady state) or `Bound → Creating` (heal) | Runs on **every** reconcile while `Bound`. `Get`s `status.newPVC` — exists → `reconcileResize`; `NotFound` → this is **self-healing**: emits a `PVCDeleted` warning event, clears the PV's stale `claimRef` (same patch pattern as `reconcileDeletingOriginal`), nils out `status.newPV`/`status.newPVC`, sets `phase = Creating`. The next loop recreates the PVC on the *same* PV — no new NAS folder |
| `reconcileDelete` | ~629 | (terminal) | Logs whether stabilization had completed; **deliberately does nothing to PV/PVC** (no OwnerReferences exist to cascade-delete through anyway); strips the finalizer and returns |

### `reconcileResize` and `sizeForNewPVC` (v0.1.9)

- `reconcileResize(vs, pvc)` runs from `reconcileBound` whenever the stable PVC exists. It compares
  `spec.target.size` with the PVC's *requested* size and *actual* `status.capacity`:
  unset → only `status.currentSize` is refreshed; greater → `client.MergeFrom` patch of
  `requests.storage` (the CSI resizer does the rest) and a poll requeue; equal → report `Resized`;
  smaller / ≤ 0 → `ResizeRejected`. A claim that is not `Bound` is deferred (the API server answers
  422 for it). `Forbidden`/`Invalid` from the API are reported as `ExpansionRejected`, never retried
  hot, and the message is cut to one stable line (validation errors embed multi-line diffs).
- It only writes status when something changed (`upsertCondition` returns a bool and never re-stamps
  an unchanged condition). **This matters:** every VS status write re-triggers a reconcile, so a
  naive "always set condition" would loop forever.
- `sizeForNewPVC(vs, pv)` returns the PV's live capacity (fallback: captured value, then 1Gi). It
  deliberately ignores `spec.target.size`: a PVC larger than its PV cannot bind.

### `reconcileDeletingOriginal` — why it's the critical phase (~line 360)

A two-step, fully resumable dance (safe to crash/restart at any point — driven entirely by re-reading
live cluster state, not in-memory flags):

1. **Delete only the source PVC** — `r.Delete(ctx, sourcePVC)` on the *claim* object, never the PV.
   Requeue after 2s. (Deleting the PV directly is what triggers the Synology CSI driver to call the
   NAS API and remove the export — see Key Design Decision #1.)
2. **Poll the released PV**: `Bound` → still propagating, requeue 2s; `Released`/`Available` with
   `spec.claimRef != nil` → patch via `client.MergeFrom(pv.DeepCopy())` setting `claimRef = nil` and
   `storageClassName = *resolveTargetStorageClassName(vs)`, requeue 2s; `claimRef == nil` already →
   fall through. Once cleared, transitions to `Creating`.
   - `client.MergeFrom` produces a minimal JSON merge patch (vs. a full `Update`), which avoids
     `resourceVersion` conflict storms on a frequently-mutated cluster-scoped object.

### `resolveTargetStorageClassName` helper (~line 692)

Single source of truth for the StorageClass decision, called from three places
(`reconcileDeletingOriginal`, `reconcileCreating`, and the healing branch of `reconcileBound`) so the
PV patch and the PVC creation always agree. Precedence:

1. `spec.target.storageClassName` if the user explicitly set it (including `""` to force fully-static,
   non-resizable binding);
2. else the StorageClass captured from the original PV (`status.originalPV.storageClassName`);
3. else `""`.

> **Behavior change (released in v0.1.8):**
> earlier released versions (≤ v0.1.7) hard-coded `""` here — fully static, non-resizable binding.
> The new default *preserves* the original StorageClass so normal PVC expansion keeps working after
> stabilization (see "Resize recovery after disk full" and the two `tests/run-*-fill-expand-recover.sh`
> scripts). Setting `spec.target.storageClassName: ""` explicitly still gets you the old behavior.

### `SetupWithManager` — watch wiring (~line 705)

- `For(&VolumeStabilization{})` — primary resource watch (standard).
- `Watches(&corev1.PersistentVolumeClaim{}, pvcMapper)` — secondary watch with a **custom mapper**,
  deliberately *not* `Owns()` (which requires an `OwnerReference`, and PVCs intentionally have none —
  see Key Design Decision #3, data-safety on VS-CR deletion). `pvcMapper` reads the
  `synology.csi.io/stabilization-cr` annotation off any PVC add/update/delete event and enqueues a
  reconcile request for the matching VS CR by name+namespace. This is the entire mechanism that makes
  self-healing event-driven (seconds) instead of poll-based.

### Status plumbing helpers

- `setCondition(vs, type, status, reason, message)` (~line 661) — upsert-by-`Type` into
  `status.conditions`, always stamping `LastTransitionTime = metav1.Now()`.
- `updateStatus(ctx, vs)` (~line 681) — sets `status.observedGeneration = vs.Generation`, calls
  `r.Status().Update`, and converts `apierrors.IsConflict` into `ctrl.Result{Requeue: true}` (the
  standard optimistic-concurrency retry — avoids surfacing benign conflicts as reconcile errors).

### API types quick reference

[operator/api/v1/volumestabilization_types.go](operator/api/v1/volumestabilization_types.go) defines
the Go types backing the CRD (mirrored 1:1 in [crds/volumestabilization-crd.yaml](crds/volumestabilization-crd.yaml)
and embedded again in [quickstart/operator.yaml](quickstart/operator.yaml) — **the schema is duplicated
in three places and must be kept in sync by hand**, there is no generation step wired into `make manifests`
pointing at these files). Notable: `TargetVolumeSpec.StorageClassName` is `*string` (pointer) specifically
so the controller can distinguish "unset → preserve original" from `"" → force static binding"` — don't
collapse it to a plain `string`. [zz_generated.deepcopy.go](operator/api/v1/zz_generated.deepcopy.go) is
controller-gen output (`DO NOT EDIT`); regenerate with `make generate` (operator-local Makefile) if the
types change.

---

## CI/CD Pipeline

[.github/workflows/ci.yml](.github/workflows/ci.yml) — triggers on push/PR to `main`, on `v*` tags, and
manual `workflow_dispatch` (with a platform picker: `linux/amd64,linux/arm64` / `amd64`-only / `arm64`-only).
Four jobs, each writing a `$GITHUB_STEP_SUMMARY` report card:

| Job | Tooling | Key behavior | Blocks the pipeline? |
|---|---|---|---|
| **Lint** | `go mod tidy` + `golangci-lint-action@v6` | HTML report artifact (`lint-report.html`, 30-day retention) | Yes |
| **Test** | `gotestsum` → `go test -race -count=1 -timeout 120s -coverprofile -covermode=atomic ./...` | JUnit XML + HTML report (`go-test-report`) + Codecov upload (`fail_ci_if_error: false`) | Yes |
| **Security** | `govulncheck-action@v1` (Go CVE scan) + `gosec` (SARIF → GitHub Security tab + HTML report) | `go get golang.org/x/net@latest` first to patch transitive http2 CVEs | No — `continue-on-error: true` on both; annotations only |
| **Build** | `needs: [lint, test, security]`; Buildx multi-arch build, `provenance: true` + `sbom: true`, Trivy image scan (SARIF + HTML, `CRITICAL,HIGH`, non-blocking), Cosign keyless signing via Sigstore/OIDC, auto `softprops/action-gh-release` on `v*` tag push | Pushes to Docker Hub (`jaderoliver/volume-stabilization-operator`) only when `github.event_name != 'pull_request'`; PRs build but never push/sign/scan-upload | Build step yes; scans no |

Image tagging (`docker/metadata-action`): branch ref, semver `{version}` and `{major}.{minor}` on tags,
and `sha-<short>`. Least-privilege `permissions: contents: read` at the workflow root; jobs that need
more (Security → `security-events: write`; Build → `contents: write`, `id-token: write`,
`security-events: write`) declare it explicitly.

---

## Project Structure

```
volume-stabilization-operator/
│
├── README.md                          # This file
├── LICENSE                            # MIT
├── volume_stabilization_architecture_board.jsx
│                                      # Standalone React/Tailwind component (self-contained,
│                                      # `export default function VolumeStabilizationArchitectureBoard()`)
│                                      # rendering an architecture/flow diagram of the operator —
│                                      # for embedding in a docs site, Storybook, or design tool.
│                                      # Not wired into any build here; purely illustrative/no deps
│                                      # beyond Tailwind utility classes.
│
├── .github/workflows/
│   └── ci.yml                         # Lint + Test + Security + multi-arch Build/Sign/Release
│                                      # — see "CI/CD Pipeline" above
│
├── quickstart/
│   ├── operator.yaml          # Single-file install: CRD + Namespace + SA + RBAC + Deployment
│   │                          # (full standalone manifest — duplicates crds/ + config/ contents
│   │                          #  so a user can `kubectl apply -f` exactly one file)
│   └── example-usage.yaml     # Step-by-step usage example with selector labels (3 steps:
│                              # dynamic-pvc → stabilize → workload)
│
├── operator/                  # Go source (controller-runtime v0.17 based)
│   ├── main.go                # Manager bootstrap: scheme registration, flags
│   │                          # (--metrics-bind-address, --health-probe-bind-address,
│   │                          # --leader-elect), zap logger, healthz/readyz probes
│   ├── Dockerfile             # Multi-stage build (golang:1.21-alpine → alpine:latest);
│   │                          # runs as UID/GID 65532 (nonroot)
│   ├── Makefile               # manifests/generate (controller-gen), fmt/vet/test/build,
│   │                          # docker-build/push, install/deploy via kustomize (../config/)
│   ├── go.mod                 # Module github.com/example/volume-stabilization-operator;
│   │                          # k8s.io/{api,apimachinery,client-go} v0.29.0, controller-runtime v0.17.0
│   ├── api/v1/
│   │   ├── volumestabilization_types.go   # CRD Go types — spec/status structs, phase enum,
│   │   │                                  # condition type. See "API types quick reference" above
│   │   ├── groupversion_info.go           # GroupVersion=synology.csi.io/v1, SchemeBuilder
│   │   └── zz_generated.deepcopy.go       # controller-gen output (DO NOT EDIT — `make generate`)
│   └── controllers/
│       ├── volumestabilization_resize_test.go # Unit tests (fake client, no cluster): resize, heal sizing,
│       │                                      # idempotency, error classification, full lifecycle
│       └── volumestabilization_controller.go  # Full reconciliation logic — state machine,
│                                              # self-healing, deletion protection.
│                                              # See "Controller Internals (Code Map)" above
│
├── config/                    # Kustomize-based install (alternative to quickstart/)
│   ├── kustomization.yaml     # namespace=volume-stabilization; image tag pin v0.1.9
│   ├── namespace.yaml         # Namespace: volume-stabilization
│   ├── serviceaccount.yaml    # ServiceAccount: volume-stabilization-operator
│   ├── rbac.yaml              # ClusterRole+Binding (VS/PV/PVC/events) +
│   │                          # namespaced Role+Binding for leader-election (configmaps/leases)
│   └── deployment.yaml        # Deployment: 1 replica, nonroot 65532, readOnlyRootFilesystem,
│                              # drops ALL caps, /healthz + /readyz probes on :8081, metrics :8080
│
├── crds/
│   └── volumestabilization-crd.yaml   # Standalone CRD manifest (kept in sync by hand with the
│                                      # Go types and the embedded copy in quickstart/operator.yaml)
│
├── tests/
│   ├── test-workload.yaml                    # E2E test: two-phase PVC + Deployment (selectors phase=1/2)
│   ├── test-volumestabilization.yaml         # Test VS CR (stabilize-my-test-data: my-data-dynamic → my-data)
│   ├── run-fill-expand-recover.sh             # Bash E2E: stabilize → fill disk → crashloop workload →
│   │                                          # resize PVC → verify recovery → scale to 2 replicas.
│   │                                          # Validates the v0.1.8 "preserve StorageClass for
│   │                                          # expansion" behavior end-to-end. Env overrides:
│   │                                          # NAMESPACE, STORAGE_CLASS, INITIAL_SIZE, TARGET_SIZE,
│   │                                          # TIMEOUT, KEEP_NAMESPACE
│   ├── run-resize-via-cr.sh                   # Declarative resize via spec.target.size (v0.1.9), 40 checks:
│   │                                          # CRD validation, grow, idempotency, shrink, heal-at-grown-size,
│   │                                          # non-expandable StorageClass. EMULATE_CSI=true for kind.
│   ├── run-upgrade-rollback.sh                # OLD operator -> NEW CRD -> NEW operator -> resize -> ROLLBACK
│   │                                          # -> roll forward; asserts the PVC is never recreated/rewritten.
│   └── run-postgres-fill-expand-recover.sh    # Same scenario but with a real PostgreSQL workload
│                                              # (init-container chowns pgdata, readiness/liveness via
│                                              # pg_isready); writes/reads rows before fill, after full-disk
│                                              # failure, after resize, and after a rollout restart —
│                                              # proves data integrity survives the whole cycle.
│                                              # Extra env: POSTGRES_IMAGE
│
└── examples/
    ├── workload-with-pvc.yaml         # Two-phase deployment pattern (PVC+stabilize, then workload)
    ├── volumestabilization.yaml       # Example VolumeStabilization CR (deletionProtection: true,
    │                                  # commented storageClassName guidance)
    ├── statefulset-example.yaml       # StatefulSet with multiple PVCs — one VS CR per replica PVC
    └── helm-pre-install-hook.yaml     # Helm integration: pre-install/pre-upgrade hook-weight pattern
                                       # (-5 seed PVC, 0 stabilize, normal workload), plus
                                       # existingClaim / Kustomize-patch / post-renderer alternatives
```
