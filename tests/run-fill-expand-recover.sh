#!/usr/bin/env bash

set -euo pipefail

NAMESPACE="${NAMESPACE:-vs-fill-expand-test}"
STORAGE_CLASS="${STORAGE_CLASS:-nfs-synology}"
INITIAL_SIZE="${INITIAL_SIZE:-1Gi}"
TARGET_SIZE="${TARGET_SIZE:-2Gi}"
TIMEOUT="${TIMEOUT:-300s}"
KEEP_NAMESPACE="${KEEP_NAMESPACE:-false}"

cleanup() {
	if [[ "${KEEP_NAMESPACE}" == "true" ]]; then
		echo "Keeping namespace ${NAMESPACE} for inspection"
		return
	fi
	kubectl delete namespace "${NAMESPACE}" --wait=false >/dev/null 2>&1 || true
}

trap cleanup EXIT

echo "Creating namespace ${NAMESPACE}"
kubectl delete namespace "${NAMESPACE}" --ignore-not-found=true >/dev/null 2>&1 || true
kubectl create namespace "${NAMESPACE}" >/dev/null

echo "Applying seed PVC and VolumeStabilization"
cat <<EOF | kubectl apply -n "${NAMESPACE}" -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data-dynamic
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: ${INITIAL_SIZE}
  storageClassName: ${STORAGE_CLASS}
---
apiVersion: synology.csi.io/v1
kind: VolumeStabilization
metadata:
  name: stabilize-my-test-data
spec:
  sourcePVC:
    name: my-data-dynamic
    namespace: ${NAMESPACE}
  target:
    pvcName: my-data
    namespace: ${NAMESPACE}
  preserveAnnotations: true
  preserveLabels: true
  waitForDeletion: true
EOF

kubectl wait -n "${NAMESPACE}" --for=jsonpath='{.status.phase}'=Bound \
	volumestabilization/stabilize-my-test-data --timeout="${TIMEOUT}"

echo "Stable PVC after stabilization"
kubectl get pvc my-data -n "${NAMESPACE}" -o wide

echo "Filling the stabilized PVC"
cat <<EOF | kubectl apply -n "${NAMESPACE}" -f -
apiVersion: v1
kind: Pod
metadata:
  name: pvc-filler
spec:
  restartPolicy: Never
  containers:
  - name: filler
    image: alpine:3.20
    command:
    - sh
    - -c
    - |
      set +e
      dd if=/dev/zero of=/data/fill.bin bs=1M count=1100
      echo "[filler] exit=$?"
      df -h /data
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: my-data
EOF

kubectl wait -n "${NAMESPACE}" --for=jsonpath='{.status.phase}'=Succeeded pod/pvc-filler --timeout="${TIMEOUT}"
kubectl logs -n "${NAMESPACE}" pvc-filler

echo "Creating workload that crashloops on startup write"
cat <<EOF | kubectl apply -n "${NAMESPACE}" -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: startup-write-check
spec:
  replicas: 1
  selector:
    matchLabels:
      app: startup-write-check
  template:
    metadata:
      labels:
        app: startup-write-check
    spec:
      containers:
      - name: app
        image: alpine:3.20
        command:
        - sh
        - -c
        - |
          set -eu
          echo "[app] startup write test"
          dd if=/dev/zero of=/data/startup.bin bs=1M count=1 oflag=append conv=notrunc status=none
          echo "[app] startup write ok"
          sleep 3600
        volumeMounts:
        - name: data
          mountPath: /data
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: my-data
EOF

kubectl wait -n "${NAMESPACE}" --for=jsonpath='{.status.containerStatuses[0].restartCount}'=1 \
	pod -l app=startup-write-check --timeout="${TIMEOUT}" || true

pod_name=$(kubectl get pods -n "${NAMESPACE}" -l app=startup-write-check -o jsonpath='{.items[0].metadata.name}')
echo "Crashloop evidence from ${pod_name}"
kubectl logs -n "${NAMESPACE}" "${pod_name}" --tail=50 || true

echo "Resizing stabilized PVC to ${TARGET_SIZE}"
kubectl patch pvc -n "${NAMESPACE}" my-data --type=merge \
	-p "{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"${TARGET_SIZE}\"}}}}"

kubectl wait -n "${NAMESPACE}" --for=jsonpath='{.status.capacity.storage}'="${TARGET_SIZE}" \
	pvc/my-data --timeout="${TIMEOUT}"

echo "PVC after resize"
kubectl get pvc my-data -n "${NAMESPACE}" -o wide

echo "Waiting for workload recovery"
kubectl wait -n "${NAMESPACE}" --for=jsonpath='{.status.readyReplicas}'=1 \
	deployment/startup-write-check --timeout="${TIMEOUT}"

echo "Scaling recovered workload to 2 replicas"
kubectl scale deployment -n "${NAMESPACE}" startup-write-check --replicas=2 >/dev/null
kubectl wait -n "${NAMESPACE}" --for=jsonpath='{.status.readyReplicas}'=2 \
	deployment/startup-write-check --timeout="${TIMEOUT}"

echo "Recovered pods"
kubectl get pods -n "${NAMESPACE}" -l app=startup-write-check -o wide
for pod in $(kubectl get pods -n "${NAMESPACE}" -l app=startup-write-check -o jsonpath='{.items[*].metadata.name}'); do
	echo "### ${pod}"
	kubectl logs -n "${NAMESPACE}" "${pod}" --tail=20 || true
done

echo "Filesystem after expansion"
kubectl exec -n "${NAMESPACE}" deploy/startup-write-check -- df -h /data