/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	synologyv1 "github.com/example/volume-stabilization-operator/api/v1"
)

const (
	volumeStabilizationFinalizer = "synology.csi.io/volumestabilization-finalizer"
	stabilizedLabel              = "synology.csi.io/stabilized"
	originalPVCNameAnnotation    = "synology.csi.io/original-pvc-name"
	originalPVNameAnnotation     = "synology.csi.io/original-pv-name"
)

// VolumeStabilizationReconciler reconciles a VolumeStabilization object
type VolumeStabilizationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=synology.csi.io,resources=volumestabilizations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=synology.csi.io,resources=volumestabilizations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=synology.csi.io,resources=volumestabilizations/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile is the main reconciliation loop for VolumeStabilization
func (r *VolumeStabilizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the VolumeStabilization instance
	vs := &synologyv1.VolumeStabilization{}
	if err := r.Get(ctx, req.NamespacedName, vs); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("VolumeStabilization resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get VolumeStabilization")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !vs.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, vs)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(vs, volumeStabilizationFinalizer) {
		controllerutil.AddFinalizer(vs, volumeStabilizationFinalizer)
		if err := r.Update(ctx, vs); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Skip if failed (requires user intervention)
	if vs.Status.Phase == synologyv1.VolumeStabilizationPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Reconcile based on phase
	switch vs.Status.Phase {
	case "":
		vs.Status.Phase = synologyv1.VolumeStabilizationPhasePending
		return r.updateStatus(ctx, vs)
	case synologyv1.VolumeStabilizationPhasePending:
		return r.reconcilePending(ctx, vs)
	case synologyv1.VolumeStabilizationPhaseDiscovering:
		return r.reconcileDiscovering(ctx, vs)
	case synologyv1.VolumeStabilizationPhaseCapturing:
		return r.reconcileCapturing(ctx, vs)
	case synologyv1.VolumeStabilizationPhaseDeletingOriginal:
		return r.reconcileDeletingOriginal(ctx, vs)
	case synologyv1.VolumeStabilizationPhaseCreating:
		return r.reconcileCreating(ctx, vs)
	case synologyv1.VolumeStabilizationPhaseBound:
		return r.reconcileBound(ctx, vs)
	default:
		log.Info("Unknown phase", "phase", vs.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *VolumeStabilizationReconciler) reconcilePending(ctx context.Context, vs *synologyv1.VolumeStabilization) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling Pending phase")

	// Determine source namespace
	sourceNamespace := vs.Spec.SourcePVC.Namespace
	if sourceNamespace == "" {
		sourceNamespace = vs.Namespace
	}

	// Check if source PVC exists
	sourcePVC := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      vs.Spec.SourcePVC.Name,
		Namespace: sourceNamespace,
	}, sourcePVC)

	if err != nil {
		if apierrors.IsNotFound(err) {
			vs.Status.Phase = synologyv1.VolumeStabilizationPhaseFailed
			vs.Status.Message = fmt.Sprintf("Source PVC %s/%s not found", sourceNamespace, vs.Spec.SourcePVC.Name)
			r.setCondition(vs, "SourcePVCExists", corev1.ConditionFalse, "NotFound", vs.Status.Message)
			r.Recorder.Eventf(vs, corev1.EventTypeWarning, "SourcePVCNotFound", "Source PVC %s/%s not found", sourceNamespace, vs.Spec.SourcePVC.Name)
			return r.updateStatus(ctx, vs)
		}
		return ctrl.Result{}, err
	}

	// Check if PVC is already stabilized
	if sourcePVC.Labels[stabilizedLabel] == "true" {
		vs.Status.Phase = synologyv1.VolumeStabilizationPhaseFailed
		vs.Status.Message = "Source PVC is already stabilized"
		r.setCondition(vs, "SourcePVCExists", corev1.ConditionFalse, "AlreadyStabilized", vs.Status.Message)
		r.Recorder.Event(vs, corev1.EventTypeWarning, "AlreadyStabilized", "Source PVC is already stabilized")
		return r.updateStatus(ctx, vs)
	}

	// Transition to Discovering
	vs.Status.Phase = synologyv1.VolumeStabilizationPhaseDiscovering
	vs.Status.Message = fmt.Sprintf("Found source PVC %s/%s", sourceNamespace, vs.Spec.SourcePVC.Name)
	r.setCondition(vs, "SourcePVCExists", corev1.ConditionTrue, "Found", vs.Status.Message)
	return r.updateStatus(ctx, vs)
}

func (r *VolumeStabilizationReconciler) reconcileDiscovering(ctx context.Context, vs *synologyv1.VolumeStabilization) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling Discovering phase")

	// Determine source namespace
	sourceNamespace := vs.Spec.SourcePVC.Namespace
	if sourceNamespace == "" {
		sourceNamespace = vs.Namespace
	}

	// Get source PVC
	sourcePVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      vs.Spec.SourcePVC.Name,
		Namespace: sourceNamespace,
	}, sourcePVC); err != nil {
		return ctrl.Result{}, err
	}

	// Check if PVC is bound
	if sourcePVC.Status.Phase != corev1.ClaimBound {
		log.Info("Source PVC not yet bound, waiting", "phase", sourcePVC.Status.Phase)
		vs.Status.Message = fmt.Sprintf("Waiting for source PVC to be bound (current: %s)", sourcePVC.Status.Phase)
		r.setCondition(vs, "SourcePVCBound", corev1.ConditionFalse, "Waiting", vs.Status.Message)
		if err := r.Status().Update(ctx, vs); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// PVC is bound, transition to Capturing
	vs.Status.Phase = synologyv1.VolumeStabilizationPhaseCapturing
	vs.Status.Message = "Source PVC is bound, capturing PV information"
	r.setCondition(vs, "SourcePVCBound", corev1.ConditionTrue, "Bound", "Source PVC is bound")
	return r.updateStatus(ctx, vs)
}

func (r *VolumeStabilizationReconciler) reconcileCapturing(ctx context.Context, vs *synologyv1.VolumeStabilization) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling Capturing phase")

	// Determine source namespace
	sourceNamespace := vs.Spec.SourcePVC.Namespace
	if sourceNamespace == "" {
		sourceNamespace = vs.Namespace
	}

	// Get source PVC
	sourcePVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      vs.Spec.SourcePVC.Name,
		Namespace: sourceNamespace,
	}, sourcePVC); err != nil {
		return ctrl.Result{}, err
	}

	// Get the bound PV
	pvName := sourcePVC.Spec.VolumeName
	if pvName == "" {
		vs.Status.Phase = synologyv1.VolumeStabilizationPhaseFailed
		vs.Status.Message = "Source PVC has no volume name"
		r.setCondition(vs, "CapturePV", corev1.ConditionFalse, "NoVolumeName", vs.Status.Message)
		return r.updateStatus(ctx, vs)
	}

	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			vs.Status.Phase = synologyv1.VolumeStabilizationPhaseFailed
			vs.Status.Message = fmt.Sprintf("PV %s not found", pvName)
			r.setCondition(vs, "CapturePV", corev1.ConditionFalse, "PVNotFound", vs.Status.Message)
			return r.updateStatus(ctx, vs)
		}
		return ctrl.Result{}, err
	}

	// Extract NFS server and path from the PV.
	// Two supported layouts:
	//   1. Native NFS PV  (spec.nfs)
	//   2. Synology CSI PV (spec.csi, driver=csi.san.synology.com, protocol=nfs)
	//      → server in volumeAttributes["dsm"]
	//      → path   in volumeAttributes["baseDir"]
	var nfsServer, nfsPath string

	switch {
	case pv.Spec.NFS != nil:
		nfsServer = pv.Spec.NFS.Server
		nfsPath = pv.Spec.NFS.Path

	case pv.Spec.CSI != nil &&
		pv.Spec.CSI.Driver == "csi.san.synology.com" &&
		pv.Spec.CSI.VolumeAttributes["protocol"] == "nfs":
		nfsServer = pv.Spec.CSI.VolumeAttributes["dsm"]
		nfsPath = pv.Spec.CSI.VolumeAttributes["baseDir"]
		if nfsServer == "" || nfsPath == "" {
			vs.Status.Phase = synologyv1.VolumeStabilizationPhaseFailed
			vs.Status.Message = fmt.Sprintf("PV %s: Synology CSI NFS PV missing 'dsm' or 'baseDir' volumeAttributes", pvName)
			r.setCondition(vs, "CapturePV", corev1.ConditionFalse, "MissingCSIAttrs", vs.Status.Message)
			return r.updateStatus(ctx, vs)
		}

	default:
		vs.Status.Phase = synologyv1.VolumeStabilizationPhaseFailed
		vs.Status.Message = fmt.Sprintf("PV %s is not a supported NFS volume (need spec.nfs or Synology CSI with protocol=nfs)", pvName)
		r.setCondition(vs, "CapturePV", corev1.ConditionFalse, "NotNFS", vs.Status.Message)
		r.Recorder.Eventf(vs, corev1.EventTypeWarning, "NotNFS", "PV %s is not a supported NFS volume", pvName)
		return r.updateStatus(ctx, vs)
	}

	// Capture PV information
	storage := pv.Spec.Capacity.Storage()
	storageStr := "1Gi"
	if storage != nil {
		storageStr = storage.String()
	}

	accessModes := make([]string, len(pv.Spec.AccessModes))
	for i, mode := range pv.Spec.AccessModes {
		accessModes[i] = string(mode)
	}

	reclaimPolicy := string(pv.Spec.PersistentVolumeReclaimPolicy)
	if reclaimPolicy == "" {
		reclaimPolicy = "Retain" // Default assumption
	}

	mountOptions := pv.Spec.MountOptions
	if mountOptions == nil {
		mountOptions = []string{"nfsvers=4.1", "hard", "noatime"}
	}

	vs.Status.OriginalPV = &synologyv1.OriginalPVInfo{
		Name:             pv.Name,
		Server:           nfsServer,
		Path:             nfsPath,
		Storage:          storageStr,
		AccessModes:      accessModes,
		StorageClassName: pv.Spec.StorageClassName,
		ReclaimPolicy:    reclaimPolicy,
		MountOptions:     mountOptions,
	}

	// Transition to DeletingOriginal
	vs.Status.Phase = synologyv1.VolumeStabilizationPhaseDeletingOriginal
	vs.Status.Message = fmt.Sprintf("Captured PV %s information, releasing original PVC", pvName)
	r.setCondition(vs, "CapturePV", corev1.ConditionTrue, "Captured", "PV information captured successfully")
	r.Recorder.Eventf(vs, corev1.EventTypeNormal, "PVCaptured", "Captured PV %s information", pvName)
	return r.updateStatus(ctx, vs)
}

func (r *VolumeStabilizationReconciler) reconcileDeletingOriginal(ctx context.Context, vs *synologyv1.VolumeStabilization) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling DeletingOriginal phase")

	// Determine source namespace
	sourceNamespace := vs.Spec.SourcePVC.Namespace
	if sourceNamespace == "" {
		sourceNamespace = vs.Namespace
	}

	// Step 1: Delete the source PVC.
	// We do NOT delete the PV. Deleting the Synology CSI PV object causes the
	// CSI driver to remove the NFS export on the NAS, making the data inaccessible.
	// Instead we release the PV by deleting only the PVC, then clear its claimRef.
	sourcePVC := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      vs.Spec.SourcePVC.Name,
		Namespace: sourceNamespace,
	}, sourcePVC)

	if err == nil {
		log.Info("Deleting source PVC", "pvc", vs.Spec.SourcePVC.Name, "namespace", sourceNamespace)
		if err := r.Delete(ctx, sourcePVC); err != nil {
			return ctrl.Result{}, err
		}
		vs.Status.Message = fmt.Sprintf("Deleting source PVC %s/%s, waiting for PV to be released", sourceNamespace, vs.Spec.SourcePVC.Name)
		r.setCondition(vs, "DeleteOriginal", corev1.ConditionFalse, "DeletingPVC", vs.Status.Message)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// Step 2: Wait for the PV to transition to Released, then clear its claimRef
	// so it becomes Available for a new PVC to bind to it.
	if vs.Status.OriginalPV != nil && vs.Status.OriginalPV.Name != "" {
		pv := &corev1.PersistentVolume{}
		if err := r.Get(ctx, types.NamespacedName{Name: vs.Status.OriginalPV.Name}, pv); err != nil {
			if apierrors.IsNotFound(err) {
				// PV already gone — unusual but not fatal; proceed to Creating
				log.Info("Original PV already gone, proceeding to Creating phase", "pv", vs.Status.OriginalPV.Name)
			} else {
				return ctrl.Result{}, err
			}
		} else {
			switch pv.Status.Phase {
			case corev1.VolumeBound:
				// PVC deletion is still propagating
				log.Info("PV still Bound, waiting for release", "pv", pv.Name)
				vs.Status.Message = "Waiting for original PV to be Released after PVC deletion"
				r.setCondition(vs, "DeleteOriginal", corev1.ConditionFalse, "WaitingRelease", vs.Status.Message)
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil

			case corev1.VolumeReleased, corev1.VolumeAvailable:
				if pv.Spec.ClaimRef != nil {
					// Clear claimRef and storageClassName so the PV becomes Available
					// and can be statically bound by a new PVC.
					log.Info("Clearing claimRef on original PV to make it Available", "pv", pv.Name)
					patch := client.MergeFrom(pv.DeepCopy())
					pv.Spec.ClaimRef = nil
					pv.Spec.StorageClassName = ""
					if err := r.Patch(ctx, pv, patch); err != nil {
						return ctrl.Result{}, err
					}
					vs.Status.Message = fmt.Sprintf("Cleared claimRef on PV %s, PV is now Available", pv.Name)
					r.setCondition(vs, "DeleteOriginal", corev1.ConditionFalse, "ClearingClaimRef", vs.Status.Message)
					return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
				}
				// claimRef already cleared — fall through to Creating
			}
		}
	}

	// PVC deleted and PV is Available — transition to Creating
	vs.Status.Phase = synologyv1.VolumeStabilizationPhaseCreating
	vs.Status.Message = "Original PVC deleted and PV released, creating stable PVC"
	r.setCondition(vs, "DeleteOriginal", corev1.ConditionTrue, "Released", "Original PVC deleted, original PV released and available")
	r.Recorder.Event(vs, corev1.EventTypeNormal, "PVReleased", "Original PVC deleted; original PV released for re-binding")
	return r.updateStatus(ctx, vs)
}

func (r *VolumeStabilizationReconciler) reconcileCreating(ctx context.Context, vs *synologyv1.VolumeStabilization) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling Creating phase")

	if vs.Status.OriginalPV == nil {
		vs.Status.Phase = synologyv1.VolumeStabilizationPhaseFailed
		vs.Status.Message = "Original PV information not captured"
		r.setCondition(vs, "CreateNew", corev1.ConditionFalse, "NoOriginalInfo", vs.Status.Message)
		return r.updateStatus(ctx, vs)
	}

	// The original PV (CSI-provisioned) is reused under its existing name.
	// We do NOT create a new PV to avoid triggering NFS export deletion on the NAS.
	originalPVName := vs.Status.OriginalPV.Name

	// Determine target namespace
	targetNamespace := vs.Spec.Target.Namespace
	if targetNamespace == "" {
		targetNamespace = vs.Spec.SourcePVC.Namespace
		if targetNamespace == "" {
			targetNamespace = vs.Namespace
		}
	}

	// Verify the original PV is Available before creating the PVC
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: originalPVName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			vs.Status.Phase = synologyv1.VolumeStabilizationPhaseFailed
			vs.Status.Message = fmt.Sprintf("Original PV %s no longer exists", originalPVName)
			r.setCondition(vs, "CreateNew", corev1.ConditionFalse, "PVGone", vs.Status.Message)
			return r.updateStatus(ctx, vs)
		}
		return ctrl.Result{}, err
	}
	if pv.Status.Phase != corev1.VolumeAvailable {
		log.Info("Waiting for original PV to become Available", "pv", originalPVName, "phase", pv.Status.Phase)
		vs.Status.Message = fmt.Sprintf("Waiting for PV %s to become Available (current: %s)", originalPVName, pv.Status.Phase)
		r.setCondition(vs, "CreateNew", corev1.ConditionFalse, "PVNotAvailable", vs.Status.Message)
		if err := r.Status().Update(ctx, vs); err != nil && !apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Check idempotency — target PVC already exists
	existingPVC := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: vs.Spec.Target.PVCName, Namespace: targetNamespace}, existingPVC)
	if err == nil {
		log.Info("Target PVC already exists, skipping", "pvc", vs.Spec.Target.PVCName, "namespace", targetNamespace)
		vs.Status.Phase = synologyv1.VolumeStabilizationPhaseBound
		vs.Status.NewPV = &synologyv1.NewPVInfo{Name: originalPVName}
		vs.Status.NewPVC = &synologyv1.NewPVCInfo{Name: existingPVC.Name, Namespace: existingPVC.Namespace}
		vs.Status.Message = fmt.Sprintf("Target PVC %s/%s already exists (idempotent)", targetNamespace, vs.Spec.Target.PVCName)
		r.setCondition(vs, "CreateNew", corev1.ConditionTrue, "AlreadyExists", vs.Status.Message)
		return r.updateStatus(ctx, vs)
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// Build labels/annotations — source PVC is already deleted so we use captured info
	labels := map[string]string{
		stabilizedLabel:                    "true",
		"synology.csi.io/managed-by":       "volume-stabilization-operator",
	}
	annotations := map[string]string{
		originalPVCNameAnnotation:          vs.Spec.SourcePVC.Name,
		originalPVNameAnnotation:           originalPVName,
		"synology.csi.io/stabilized-at":    time.Now().Format(time.RFC3339),
		"synology.csi.io/stabilization-cr": vs.Name,
	}

	// Parse access modes from captured info
	accessModes := make([]corev1.PersistentVolumeAccessMode, len(vs.Status.OriginalPV.AccessModes))
	for i, mode := range vs.Status.OriginalPV.AccessModes {
		switch mode {
		case "ReadWriteOnce":
			accessModes[i] = corev1.ReadWriteOnce
		case "ReadOnlyMany":
			accessModes[i] = corev1.ReadOnlyMany
		case "ReadWriteOncePod":
			accessModes[i] = corev1.ReadWriteOncePod
		default:
			accessModes[i] = corev1.ReadWriteMany
		}
	}

	// storageClassName must be "" (empty string pointer) for static pre-binding.
	// This matches the "" we set on the PV during claimRef clearing.
	empty := ""
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        vs.Spec.Target.PVCName,
			Namespace:   targetNamespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: accessModes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(vs.Status.OriginalPV.Storage),
				},
			},
			VolumeName:       originalPVName, // Pre-bind to the existing PV
			StorageClassName: &empty,         // Empty = static binding
		},
	}

	if err := r.Create(ctx, pvc); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating target PVC %s/%s: %w", targetNamespace, vs.Spec.Target.PVCName, err)
	}
	log.Info("Created stable target PVC", "pvc", pvc.Name, "namespace", pvc.Namespace, "boundTo", originalPVName)

	sourcePVCNamespace := vs.Spec.SourcePVC.Namespace
	if sourcePVCNamespace == "" {
		sourcePVCNamespace = vs.Namespace
	}

	vs.Status.NewPV = &synologyv1.NewPVInfo{Name: originalPVName}
	vs.Status.NewPVC = &synologyv1.NewPVCInfo{Name: pvc.Name, Namespace: pvc.Namespace}
	vs.Status.Phase = synologyv1.VolumeStabilizationPhaseBound
	vs.Status.Message = fmt.Sprintf("Volume stabilized: PV=%s, PVC=%s/%s", originalPVName, targetNamespace, vs.Spec.Target.PVCName)
	r.setCondition(vs, "CreateNew", corev1.ConditionTrue, "Created", vs.Status.Message)
	r.setCondition(vs, "Stabilized", corev1.ConditionTrue, "Success", "Volume stabilization completed successfully")
	r.Recorder.Eventf(vs, corev1.EventTypeNormal, "Stabilized", "Volume stabilized: %s/%s -> %s/%s",
		vs.Spec.SourcePVC.Name, sourcePVCNamespace, vs.Spec.Target.PVCName, targetNamespace)

	return r.updateStatus(ctx, vs)
}

// reconcileBound runs on every reconcile when the VS is in the Bound (terminal-success) phase.
// Its only job is self-healing: if the stable PVC was deleted externally, it clears the stale
// claimRef from the original PV and drops back to Creating so the PVC is re-created without
// touching the NAS — no new PV, no new NFS folder.
func (r *VolumeStabilizationReconciler) reconcileBound(ctx context.Context, vs *synologyv1.VolumeStabilization) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	if vs.Status.NewPVC == nil {
		// Nothing to check
		return ctrl.Result{}, nil
	}

	// Check whether the stable PVC still exists.
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: vs.Status.NewPVC.Name, Namespace: vs.Status.NewPVC.Namespace}, pvc)
	if err == nil {
		// PVC is healthy — nothing to do.
		return ctrl.Result{}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// PVC was deleted externally. Heal by re-creating it.
	log.Info("Stable PVC was deleted externally; healing",
		"pvc", vs.Status.NewPVC.Name,
		"namespace", vs.Status.NewPVC.Namespace)
	r.Recorder.Eventf(vs, corev1.EventTypeWarning, "PVCDeleted",
		"Stable PVC %s/%s was deleted; re-creating to restore binding",
		vs.Status.NewPVC.Namespace, vs.Status.NewPVC.Name)

	// The original PV is now in Released state with a stale claimRef.
	// Clear it so the PV becomes Available and can be statically rebound.
	if vs.Status.OriginalPV != nil && vs.Status.OriginalPV.Name != "" {
		pv := &corev1.PersistentVolume{}
		if pvErr := r.Get(ctx, types.NamespacedName{Name: vs.Status.OriginalPV.Name}, pv); pvErr == nil {
			if pv.Spec.ClaimRef != nil {
				log.Info("Clearing stale claimRef on original PV", "pv", pv.Name)
				patch := client.MergeFrom(pv.DeepCopy())
				pv.Spec.ClaimRef = nil
				pv.Spec.StorageClassName = ""
				if patchErr := r.Patch(ctx, pv, patch); patchErr != nil {
					return ctrl.Result{}, patchErr
				}
			}
		}
	}

	// Reset status and drop back to Creating so the PVC is recreated.
	vs.Status.Phase = synologyv1.VolumeStabilizationPhaseCreating
	vs.Status.Message = "Stable PVC was deleted externally; re-creating"
	vs.Status.NewPV = nil
	vs.Status.NewPVC = nil
	r.setCondition(vs, "Stabilized", corev1.ConditionFalse, "PVCDeleted", "Stable PVC deleted; healing by re-creating")
	return r.updateStatus(ctx, vs)
}

func (r *VolumeStabilizationReconciler) reconcileDelete(ctx context.Context, vs *synologyv1.VolumeStabilization) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling deletion")

	if !controllerutil.ContainsFinalizer(vs, volumeStabilizationFinalizer) {
		return ctrl.Result{}, nil
	}

	// Note: We intentionally do NOT delete the stabilized PV/PVC to prevent data loss.
	// The user must manually delete them if they want to remove the data.
	if vs.Status.NewPV != nil && vs.Status.NewPVC != nil {
		log.Info("Retaining stabilized PV and PVC for data safety",
			"pv", vs.Status.NewPV.Name,
			"pvc", vs.Status.NewPVC.Name,
			"namespace", vs.Status.NewPVC.Namespace)
	} else {
		log.Info("Stabilization did not complete, no PV/PVC to retain")
	}

	// PVs are cluster-scoped so they couldn't have OwnerReferences pointing to this CR.
	// PVCs have no owner reference set either (same reason — we never set one).
	// Nothing to clean up here; data is intentionally retained on the NAS.

	// Remove finalizer
	controllerutil.RemoveFinalizer(vs, volumeStabilizationFinalizer)
	if err := r.Update(ctx, vs); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *VolumeStabilizationReconciler) setCondition(vs *synologyv1.VolumeStabilization, condType string, status corev1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, cond := range vs.Status.Conditions {
		if cond.Type == condType {
			vs.Status.Conditions[i].Status = status
			vs.Status.Conditions[i].Reason = reason
			vs.Status.Conditions[i].Message = message
			vs.Status.Conditions[i].LastTransitionTime = &now
			return
		}
	}
	vs.Status.Conditions = append(vs.Status.Conditions, synologyv1.VolumeStabilizationCondition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: &now,
	})
}

func (r *VolumeStabilizationReconciler) updateStatus(ctx context.Context, vs *synologyv1.VolumeStabilization) (ctrl.Result, error) {
	vs.Status.ObservedGeneration = vs.Generation
	if err := r.Status().Update(ctx, vs); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VolumeStabilizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// We do NOT use Owns() for PVs: PVs are cluster-scoped and cannot have a
	// namespaced OwnerReference pointing to a VolumeStabilization CR.
	// We do NOT use Owns() for PVCs either: we intentionally do not set owner
	// references on the stabilized PVC so it survives VS CR deletion.
	//
	// However we DO watch PVCs: when a stabilized PVC is deleted externally,
	// we need to re-trigger reconciliation so reconcileBound can heal it.
	// The watch maps the PVC's "synology.csi.io/stabilization-cr" annotation
	// back to the owning VS CR — no OwnerReference needed.
	pvcMapper := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, obj client.Object) []reconcile.Request {
			crName, ok := obj.GetAnnotations()["synology.csi.io/stabilization-cr"]
			if !ok || crName == "" {
				return nil
			}
			return []reconcile.Request{
				{NamespacedName: types.NamespacedName{
					Name:      crName,
					Namespace: obj.GetNamespace(),
				}},
			}
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&synologyv1.VolumeStabilization{}).
		Watches(&corev1.PersistentVolumeClaim{}, pvcMapper).
		Complete(r)
}
