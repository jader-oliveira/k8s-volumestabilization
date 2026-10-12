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
	"strings"
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

	// Resize-related status conditions.
	conditionResized        = "Resized"        // True once PVC capacity >= spec.target.size
	conditionResizeRejected = "ResizeRejected" // True while spec.target.size cannot be honoured

	// How often to re-check a resize that the CSI driver has not finished yet.
	resizePollInterval = 15 * time.Second
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
		if vs.Spec.DeletionProtection {
			// Check whether the DeletionBlocked condition is already recorded.
			// If not, persist it now so it is always visible in kubectl describe
			// (events are ephemeral and easily missed; a status condition is not).
			alreadyBlocked := false
			for _, c := range vs.Status.Conditions {
				if c.Type == "DeletionBlocked" && c.Status == corev1.ConditionTrue {
					alreadyBlocked = true
					break
				}
			}
			if !alreadyBlocked {
				// First time we detect a blocked deletion — record it persistently.
				r.setCondition(vs, "DeletionBlocked", corev1.ConditionTrue, "ProtectionEnabled",
					"Deletion attempted but blocked by spec.deletionProtection=true. "+
						"Set spec.deletionProtection=false to allow deletion.")
				r.Recorder.Eventf(vs, corev1.EventTypeWarning, "DeletionProtected",
					"Deletion blocked: spec.deletionProtection=true. Set it to false to allow deletion.")
				// updateStatus triggers an immediate re-reconcile; the phase switch
				// (and reconcileBound self-healing) will run on the very next loop.
				return r.updateStatus(ctx, vs)
			}
			// Condition already recorded — fall through to normal phase reconciliation
			// so reconcileBound keeps self-healing (e.g. re-creates a deleted PVC)
			// while the VS is stuck in Terminating.
			log.Info("Deletion blocked by deletionProtection=true; continuing phase reconciliation",
				"vs", vs.Name, "namespace", vs.Namespace)
		} else {
			return r.reconcileDelete(ctx, vs)
		}
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

	// Skip if permanently failed (requires user intervention — e.g. source already stabilized)
	// Transient failures (source PVC not yet created) self-heal via requeue.
	if vs.Status.Phase == synologyv1.VolumeStabilizationPhaseFailed {
		if vs.Status.Message != "" && vs.Status.Conditions != nil {
			for _, c := range vs.Status.Conditions {
				if c.Reason == "AlreadyStabilized" {
					return ctrl.Result{}, nil
				}
			}
		}
		// Transient failure — reset to Pending so we retry
		vs.Status.Phase = synologyv1.VolumeStabilizationPhasePending
		return r.updateStatus(ctx, vs)
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
			// Source PVC doesn't exist yet — could be applied out of order; retry.
			vs.Status.Phase = synologyv1.VolumeStabilizationPhaseFailed
			vs.Status.Message = fmt.Sprintf("Source PVC %s/%s not found — waiting for it to be created", sourceNamespace, vs.Spec.SourcePVC.Name)
			r.setCondition(vs, "SourcePVCExists", corev1.ConditionFalse, "NotFound", vs.Status.Message)
			r.Recorder.Eventf(vs, corev1.EventTypeWarning, "SourcePVCNotFound", "Source PVC %s/%s not found", sourceNamespace, vs.Spec.SourcePVC.Name)
			if _, err := r.updateStatus(ctx, vs); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
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
	targetStorageClassName := resolveTargetStorageClassName(vs)

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
					// Clear claimRef so the PV becomes Available again. Keep the
					// original/requested storage class when possible so the rebound
					// PVC remains expansion-compatible.
					log.Info("Clearing claimRef on original PV to make it Available", "pv", pv.Name)
					patch := client.MergeFrom(pv.DeepCopy())
					pv.Spec.ClaimRef = nil
					pv.Spec.StorageClassName = *targetStorageClassName
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
	targetStorageClassName := resolveTargetStorageClassName(vs)

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
		stabilizedLabel:              "true",
		"synology.csi.io/managed-by": "volume-stabilization-operator",
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
					corev1.ResourceStorage: sizeForNewPVC(vs, pv),
				},
			},
			VolumeName:       originalPVName,
			StorageClassName: targetStorageClassName,
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
		// PVC is healthy — reconcile its size against spec.target.size.
		return r.reconcileResize(ctx, vs, pvc)
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
				targetStorageClassName := resolveTargetStorageClassName(vs)
				log.Info("Clearing stale claimRef on original PV", "pv", pv.Name)
				patch := client.MergeFrom(pv.DeepCopy())
				pv.Spec.ClaimRef = nil
				pv.Spec.StorageClassName = *targetStorageClassName
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

func resolveTargetStorageClassName(vs *synologyv1.VolumeStabilization) *string {
	if vs.Spec.Target.StorageClassName != nil {
		return vs.Spec.Target.StorageClassName
	}
	if vs.Status.OriginalPV != nil && vs.Status.OriginalPV.StorageClassName != "" {
		storageClassName := vs.Status.OriginalPV.StorageClassName
		return &storageClassName
	}
	empty := ""
	return &empty
}

// sizeForNewPVC returns the capacity a (re)created stable PVC must request.
//
// It is the live capacity of the PV and deliberately NOT spec.target.size:
//   - a PVC that requests more than its PV offers can never bind, and a volume
//     can only be expanded once its PVC is bound; so growth is always a second
//     step (reconcileResize) after the claim is Bound;
//   - the capacity captured when the volume was first stabilized goes stale after
//     an expansion, so it is only a fallback for PVs that report no capacity.
func sizeForNewPVC(vs *synologyv1.VolumeStabilization, pv *corev1.PersistentVolume) resource.Quantity {
	size := pv.Spec.Capacity[corev1.ResourceStorage]
	if size.IsZero() && vs.Status.OriginalPV != nil && vs.Status.OriginalPV.Storage != "" {
		if q, err := resource.ParseQuantity(vs.Status.OriginalPV.Storage); err == nil {
			size = q
		}
	}
	if size.IsZero() {
		size = resource.MustParse("1Gi")
	}
	return size
}

// reconcileResize drives the stable PVC to spec.target.size.
//
//   - size unset            → not managed; only status.currentSize is kept fresh
//   - size  > requested     → patch PVC requests.storage (CSI resizer does the rest)
//   - size == requested     → nothing to patch; report Resized once capacity catches up
//   - size  < requested     → rejected (volumes cannot shrink), PVC untouched
//
// It is idempotent and only writes VS status when something actually changed,
// because every status write re-triggers a reconcile.
func (r *VolumeStabilizationReconciler) reconcileResize(ctx context.Context, vs *synologyv1.VolumeStabilization, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Only a Bound claim can be expanded (the API server answers 422 otherwise).
	// The PVC watch re-triggers us the moment it binds; the requeue is a safety net.
	if vs.Spec.Target.Size != nil && pvc.Status.Phase != corev1.ClaimBound {
		log.Info("Stable PVC is not Bound yet; deferring resize", "pvc", pvc.Name, "phase", pvc.Status.Phase)
		return ctrl.Result{RequeueAfter: resizePollInterval}, nil
	}

	requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	actual := requested
	capacity, hasCapacity := pvc.Status.Capacity[corev1.ResourceStorage]
	if hasCapacity {
		actual = capacity
	}

	changed := false
	if cur := actual.String(); vs.Status.CurrentSize != cur {
		vs.Status.CurrentSize = cur
		changed = true
	}

	var requeueAfter time.Duration
	desired := vs.Spec.Target.Size

	switch {
	case desired == nil:
		// Size is not managed by this CR; drop any stale resize conditions.
		changed = removeCondition(vs, conditionResized) || changed
		changed = removeCondition(vs, conditionResizeRejected) || changed

	case desired.Sign() <= 0:
		changed = r.rejectResize(vs, "InvalidSize",
			"spec.target.size must be greater than zero") || changed

	case desired.Cmp(requested) < 0:
		changed = r.rejectResize(vs, "ShrinkNotSupported", fmt.Sprintf(
			"spec.target.size %s is smaller than the current size %s; volumes can only grow",
			desired.String(), requested.String())) || changed

	case desired.Cmp(requested) > 0:
		from := requested.String()
		patch := client.MergeFrom(pvc.DeepCopy())
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = desired.DeepCopy()
		if err := r.Patch(ctx, pvc, patch); err != nil {
			switch {
			case apierrors.IsConflict(err):
				return ctrl.Result{Requeue: true}, nil
			case apierrors.IsForbidden(err), apierrors.IsInvalid(err):
				// e.g. StorageClass does not allow volume expansion. Not retryable
				// until the user changes something, so report it and stop.
				log.Info("PVC expansion rejected by the API server", "pvc", pvc.Name, "error", err.Error())
				changed = r.rejectResize(vs, "ExpansionRejected", conciseAPIError(err)) || changed
			default:
				return ctrl.Result{}, err
			}
		} else {
			log.Info("Expanding stable PVC", "pvc", pvc.Name, "namespace", pvc.Namespace,
				"from", from, "to", desired.String())
			r.Recorder.Eventf(vs, corev1.EventTypeNormal, "Resizing",
				"Expanding PVC %s/%s from %s to %s", pvc.Namespace, pvc.Name, from, desired.String())
			changed = removeCondition(vs, conditionResizeRejected) || changed
			changed = upsertCondition(vs, conditionResized, corev1.ConditionFalse, "Resizing",
				fmt.Sprintf("Expanding from %s to %s", from, desired.String())) || changed
			requeueAfter = resizePollInterval
		}

	default: // desired == requested
		changed = removeCondition(vs, conditionResizeRejected) || changed
		if hasCapacity && capacity.Cmp(*desired) >= 0 {
			changed = upsertCondition(vs, conditionResized, corev1.ConditionTrue, "Resized",
				fmt.Sprintf("Volume capacity is %s", capacity.String())) || changed
		} else {
			changed = upsertCondition(vs, conditionResized, corev1.ConditionFalse, "Resizing",
				fmt.Sprintf("Waiting for the CSI driver to expand the volume to %s", desired.String())) || changed
			requeueAfter = resizePollInterval
		}
	}

	if !changed {
		if requeueAfter > 0 {
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		return ctrl.Result{}, nil
	}
	res, err := r.updateStatus(ctx, vs)
	if err != nil || res.Requeue {
		return res, err
	}
	if requeueAfter > 0 {
		res.RequeueAfter = requeueAfter
	}
	return res, nil
}

// conciseAPIError reduces an API error to its first line, capped in length.
// Validation errors can embed a multi-line object diff; putting that in a status
// condition would be unreadable and — worse — unstable between reconciles.
func conciseAPIError(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const max = 240
	if len(msg) > max {
		msg = msg[:max] + "…"
	}
	return msg
}

// rejectResize records the ResizeRejected condition and, only when that is new
// information, emits a Warning event. It reports whether status changed.
func (r *VolumeStabilizationReconciler) rejectResize(vs *synologyv1.VolumeStabilization, reason, message string) bool {
	changed := upsertCondition(vs, conditionResizeRejected, corev1.ConditionTrue, reason, message)
	if changed {
		r.Recorder.Event(vs, corev1.EventTypeWarning, reason, message)
	}
	// A rejected request means the volume is not being resized.
	removeCondition(vs, conditionResized)
	return changed
}

// upsertCondition sets a condition and reports whether anything changed.
// Unlike setCondition it never re-stamps an unchanged condition, so callers can
// use the return value to avoid pointless status writes (and reconcile loops).
// LastTransitionTime only moves when the status value itself flips.
func upsertCondition(vs *synologyv1.VolumeStabilization, condType string, status corev1.ConditionStatus, reason, message string) bool {
	now := metav1.Now()
	for i := range vs.Status.Conditions {
		c := &vs.Status.Conditions[i]
		if c.Type != condType {
			continue
		}
		if c.Status == status && c.Reason == reason && c.Message == message {
			return false
		}
		if c.Status != status {
			c.LastTransitionTime = &now
		}
		c.Status, c.Reason, c.Message = status, reason, message
		return true
	}
	vs.Status.Conditions = append(vs.Status.Conditions, synologyv1.VolumeStabilizationCondition{
		Type: condType, Status: status, Reason: reason, Message: message, LastTransitionTime: &now,
	})
	return true
}

// removeCondition deletes a condition and reports whether it existed.
func removeCondition(vs *synologyv1.VolumeStabilization, condType string) bool {
	for i := range vs.Status.Conditions {
		if vs.Status.Conditions[i].Type == condType {
			vs.Status.Conditions = append(vs.Status.Conditions[:i], vs.Status.Conditions[i+1:]...)
			return true
		}
	}
	return false
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
