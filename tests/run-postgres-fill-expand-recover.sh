#!/usr/bin/env bash

set -euo pipefail

NAMESPACE="${NAMESPACE:-vs-postgres-expand-test}"
STORAGE_CLASS="${STORAGE_CLASS:-nfs-synology}"
INITIAL_SIZE="${INITIAL_SIZE:-1Gi}"
TARGET_SIZE="${TARGET_SIZE:-2Gi}"
TIMEOUT="${TIMEOUT:-300s}"
KEEP_NAMESPACE="${KEEP_NAMESPACE:-false}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16-alpine}"

cleanup() {
	if [[ "${KEEP_NAMESPACE}" == "true" ]]; then
		echo "Keeping namespace ${NAMESPACE} for inspection"
		return
	fi
	kubectl delete namespace "${NAMESPACE}" --wait=false >/dev/null 2>&1 || true
}

trap cleanup EXIT

sql_exec() {
	local sql="$1"
	kubectl exec -n "${NAMESPACE}" deploy/postgres -- sh -lc "psql -v ON_ERROR_STOP=1 -U postgres -d postgres -c \"${sql}\""
}

echo "Creating namespace ${NAMESPACE}"
kubectl delete namespace "${NAMESPACE}" --ignore-not-found=true >/dev/null 2>&1 || true
kubectl create namespace "${NAMESPACE}" >/dev/null

echo "Applying seed PVC and VolumeStabilization"
cat <<EOF | kubectl apply -n "${NAMESPACE}" -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: pg-data-dynamic
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
  name: stabilize-pg-data
spec:
  sourcePVC:
    name: pg-data-dynamic
    namespace: ${NAMESPACE}
  target:
    pvcName: pg-data
    namespace: ${NAMESPACE}
  preserveAnnotations: true
  preserveLabels: true
  waitForDeletion: true
EOF

kubectl wait -n "${NAMESPACE}" --for=jsonpath='{.status.phase}'=Bound volumestabilization/stabilize-pg-data --timeout="${TIMEOUT}"

echo "Deploying PostgreSQL on stabilized PVC"
cat <<EOF | kubectl apply -n "${NAMESPACE}" -f -
apiVersion: v1
kind: Service
metadata:
  name: postgres
spec:
  selector:
    app: postgres
  ports:
  - name: postgres
    port: 5432
    targetPort: 5432
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      securityContext:
        runAsUser: 70
        runAsGroup: 0
      initContainers:
      - name: init-pgdata
        image: alpine:3.20
        securityContext:
          runAsUser: 0
          runAsGroup: 0
        command:
        - sh
        - -c
        - |
          mkdir -p /var/lib/postgresql/data/pgdata
          chown 70:0 /var/lib/postgresql/data/pgdata
          chmod 700 /var/lib/postgresql/data/pgdata
        volumeMounts:
        - name: data
          mountPath: /var/lib/postgresql/data
      containers:
      - name: postgres
        image: ${POSTGRES_IMAGE}
        env:
        - name: POSTGRES_PASSWORD
          value: postgres
        - name: PGDATA
          value: /var/lib/postgresql/data/pgdata
        ports:
        - containerPort: 5432
          name: postgres
        readinessProbe:
          exec:
            command:
            - sh
            - -c
            - pg_isready -U postgres -d postgres
          initialDelaySeconds: 10
          periodSeconds: 5
        livenessProbe:
          exec:
            command:
            - sh
            - -c
            - pg_isready -U postgres -d postgres
          initialDelaySeconds: 30
          periodSeconds: 10
        volumeMounts:
        - name: data
          mountPath: /var/lib/postgresql/data
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: pg-data
EOF

kubectl rollout status -n "${NAMESPACE}" deployment/postgres --timeout="${TIMEOUT}"

echo "Baseline PostgreSQL state"
sql_exec "CREATE TABLE IF NOT EXISTS fill_test (id bigserial primary key, payload text, created_at timestamptz default now());"
sql_exec "INSERT INTO fill_test(payload) VALUES ('baseline-write');"
sql_exec "SELECT count(*) AS baseline_rows FROM fill_test;"

echo "Filling the volume from a separate pod"
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
      dd if=/dev/zero of=/data/filler.bin bs=1M count=1100
      echo "[filler] exit=$?"
      df -h /data
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: pg-data
EOF

kubectl wait -n "${NAMESPACE}" --for=jsonpath='{.status.phase}'=Succeeded pod/pvc-filler --timeout="${TIMEOUT}"
kubectl logs -n "${NAMESPACE}" pvc-filler

echo "Triggering a PostgreSQL write after the volume is full"
set +e
sql_exec "INSERT INTO fill_test(payload) SELECT md5(random()::text || clock_timestamp()::text) FROM generate_series(1,250000);"
write_exit_code=$?
set -e
echo "Postgres write exit code after full disk: ${write_exit_code}"

echo "Collecting PostgreSQL failure evidence"
kubectl logs -n "${NAMESPACE}" deploy/postgres --tail=200
kubectl get pod -n "${NAMESPACE}" -l app=postgres -o wide
kubectl get pod -n "${NAMESPACE}" -l app=postgres -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}{"\n"}'
kubectl exec -n "${NAMESPACE}" deploy/postgres -- df -h /var/lib/postgresql/data || true

echo "Resizing stabilized PVC to ${TARGET_SIZE}"
kubectl patch pvc -n "${NAMESPACE}" pg-data --type=merge -p "{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"${TARGET_SIZE}\"}}}}"
kubectl wait -n "${NAMESPACE}" --for=jsonpath='{.status.capacity.storage}'="${TARGET_SIZE}" pvc/pg-data --timeout="${TIMEOUT}"

echo "Post-resize PVC and filesystem"
kubectl get pvc pg-data -n "${NAMESPACE}" -o wide
kubectl exec -n "${NAMESPACE}" deploy/postgres -- df -h /var/lib/postgresql/data || true

echo "Retrying PostgreSQL writes after resize"
sql_exec "INSERT INTO fill_test(payload) VALUES ('after-resize-write');"
sql_exec "SELECT count(*) AS rows_after_resize FROM fill_test;"

echo "Restarting PostgreSQL once to confirm clean restart on the expanded volume"
kubectl rollout restart -n "${NAMESPACE}" deployment/postgres
kubectl rollout status -n "${NAMESPACE}" deployment/postgres --timeout="${TIMEOUT}"
sql_exec "SELECT count(*) AS rows_after_restart FROM fill_test;"
kubectl logs -n "${NAMESPACE}" deploy/postgres --tail=120

echo "Test completed successfully"