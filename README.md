# Volume Stabilization Operator

> A Kubernetes operator that gives dynamically-provisioned NFS volumes a **stable, predictable PVC name** — without recreating the volume or touching data on the NAS.

[![Go](https://img.shields.io/badge/Go-1.21-blue)](https://golang.org)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.27%2B-blue)](https://kubernetes.io)
[![controller-runtime](https://img.shields.io/badge/controller--runtime-v0.17-blue)](https://github.com/kubernetes-sigs/controller-runtime)
[![Image](https://img.shields.io/badge/image-jaderoliver%2Fvolume--stabilization--operator%3Av0.1.4-blue)](https://hub.docker.com/r/jaderoliver/volume-stabilization-operator)
[![License](https://img.shields.io/badge/License-Apache%202.0-green)](https://www.apache.org/licenses/LICENSE-2.0)

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
- [Compatibility](#compatibility)
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
3. **Patch the PV** in-place: set `spec.claimRef = nil` and `spec.storageClassName = ""`
4. The PV transitions to `Available` — ready to be statically bound

No NFS export is touched. No NAS API call is made. The folder at `/volume1/k8s-csi-pvc-<uuid>` is untouched.

### Reconciliation Detail: Creating Phase

Creates a new PVC that statically pre-binds to the original PV:

```yaml
spec:
  volumeName: pvc-3892bba3-75e2-4a59-99c6-07b3652a98b9  # original PV
  storageClassName: ""                                    # bypass dynamic provisioning
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 1Gi
```

Kubernetes binds this PVC to the PV immediately — no new provisioning, no StorageClass involved.

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

### 2. Static binding via empty StorageClassName

Setting `spec.storageClassName: ""` (empty string — not `null`, not omitted) on a PVC prevents the dynamic provisioner from acting on it and allows Kubernetes to bind it to a specific PV via `spec.volumeName`. This is a precise Kubernetes API contract.

### 3. No OwnerReferences on PV or PVC

PVs are cluster-scoped; a namespaced CR cannot own a cluster-scoped object — Kubernetes rejects such OwnerReferences. PVCs intentionally have no OwnerReference so they **survive VS CR deletion** (data safety). The relationship is tracked via annotations (`synology.csi.io/stabilization-cr`) and VS status fields.

### 4. Two-phase deployment pattern

The source PVC must be created and bound **before** the VS CR is applied, and the workload **after** stabilization completes. This is the necessary ordering — it cannot be parallelized. This maps cleanly to Helm pre-install hooks and GitOps wave ordering.

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
    storageClassName: ""         # optional: leave unset for static binding (recommended)

  preserveAnnotations: true      # optional: copy annotations to new PVC (default: true)
  preserveLabels: true           # optional: copy labels to new PVC (default: true)
  waitForDeletion: true          # optional: wait for full PVC deletion (default: true)
```

### Status Fields

```yaml
status:
  phase: Bound                   # Pending | Discovering | Capturing | DeletingOriginal
                                 # | Creating | Bound | Failed
  message: "Volume stabilized: PV=pvc-3892..., PVC=default/my-app-data"

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
| Source PVC re-created after stabilization | Ignored (already `phase=Bound`) | ✅ |
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

### Removing everything cleanly

```bash
# Delete workload first
kubectl delete deployment my-app -n default

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

## Project Structure

```
volume-stabilization-operator/
│
├── quickstart/
│   ├── operator.yaml          # Single-file install: CRD + RBAC + Deployment
│   └── example-usage.yaml     # Step-by-step usage example with selector labels
│
├── operator/                  # Go source (controller-runtime based)
│   ├── main.go                # Manager bootstrap, flags, leader election
│   ├── Dockerfile             # Multi-stage build; runs as UID 65532 (nonroot)
│   ├── Makefile               # Build, push, and deploy targets
│   ├── go.mod                 # Go module definition
│   ├── api/v1/
│   │   ├── volumestabilization_types.go   # CRD Go types
│   │   ├── groupversion_info.go           # API group registration
│   │   └── zz_generated.deepcopy.go      # Generated DeepCopy methods
│   └── controllers/
│       └── volumestabilization_controller.go  # Full reconciliation logic
│
├── config/                    # Kustomize-based install (alternative to quickstart/)
│   ├── kustomization.yaml
│   ├── namespace.yaml
│   ├── serviceaccount.yaml
│   ├── rbac.yaml
│   └── deployment.yaml
│
├── crds/
│   └── volumestabilization-crd.yaml   # Standalone CRD manifest
│
├── tests/
│   ├── test-workload.yaml             # End-to-end test: two-phase PVC + Deployment
│   └── test-volumestabilization.yaml  # Test VS CR
│
└── examples/
    ├── workload-with-pvc.yaml         # Two-phase deployment pattern
    ├── volumestabilization.yaml       # Example VolumeStabilization CR
    ├── statefulset-example.yaml       # StatefulSet with multiple PVCs
    └── helm-pre-install-hook.yaml     # Helm integration patterns
```
