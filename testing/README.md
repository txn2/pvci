# PVC Creation and Injection

The following sequence works with the Ceph CSI driver
`quay.io/cephcsi/cephcsi:v3.2.2` or greater.

View supported storage classes: `kubectl get storageclass`

Overview:
1. Create `rook-ceph-block` PVC with access mode `RWO` (Read-Write Only).
2. Create Job to inject objects into the new `RWO` PVC.
3. Create `rook-ceph-block` PVC with access mode `ROX` (Read-Only Many) and set the
   first volume as the "content source"

Manual Seps using Kubernetes Manifests:

Create a `pvci-test` namespace:
- `kubectl apply -f 000-namespace.yml`

Create a single node MinIO service:
- `kubectl apply -f 005-minio-service.yml`
- `kubectl apply -f 010-minio-deployment.yml`

Populate MinIO with generated sample text files:
- `kubectl apply -f 015-populate-minio-job.yml`

Create a RWO (Read-write Once) PVC:
- `kubectl apply -f 100-manual-pvc-rwo.yml`

Populate the RWO PVC with data from MinIO:
- `kubectl apply -f 110-maunual-populate-pvc-job.yml`

Create a new ROX (Read-only Many) PVC from the previous RWO PVC:
- `kubectl apply -f 120-manual-pvc-from-pvc.yml`

Attach two Pods to the new ROX PVC:
- `kubectl apply -f 130-test-pod.yml`
- `kubectl apply -f 140-test-pod2.yml`

