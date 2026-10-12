package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	synologyv1 "github.com/example/volume-stabilization-operator/api/v1"
)

const (
	testNS    = "database"
	testVS    = "stabilize-pg-0"
	testPVC   = "pgdata-pg-0"
	testPV    = "pvc-original-uuid"
	testClass = "nfs-synology"
)

// ---------------------------------------------------------------- fixtures --

func q(s string) resource.Quantity { return resource.MustParse(s) }
func qp(s string) *resource.Quantity {
	v := resource.MustParse(s)
	return &v
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := synologyv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// boundVS is a fully stabilized VS whose stable PVC is testNS/testPVC.
// originalPV.storage is deliberately stale (15Gi) in several tests.
func boundVS(size *resource.Quantity) *synologyv1.VolumeStabilization {
	return &synologyv1.VolumeStabilization{
		ObjectMeta: metav1.ObjectMeta{
			Name: testVS, Namespace: testNS, Generation: 1,
			Finalizers: []string{volumeStabilizationFinalizer},
		},
		Spec: synologyv1.VolumeStabilizationSpec{
			SourcePVC: synologyv1.SourcePVCReference{Name: "pg-dynamic-0"},
			Target:    synologyv1.TargetVolumeSpec{PVCName: testPVC, Size: size},
		},
		Status: synologyv1.VolumeStabilizationStatus{
			Phase: synologyv1.VolumeStabilizationPhaseBound,
			OriginalPV: &synologyv1.OriginalPVInfo{
				Name: testPV, Storage: "15Gi", StorageClassName: testClass,
				AccessModes: []string{"ReadWriteOnce"},
			},
			NewPV:  &synologyv1.NewPVInfo{Name: testPV},
			NewPVC: &synologyv1.NewPVCInfo{Name: testPVC, Namespace: testNS},
		},
	}
}

// stablePVC is the operator-created PVC. capacity "" means status.capacity unset.
func stablePVC(request, capacity string) *corev1.PersistentVolumeClaim {
	sc := testClass
	p := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testPVC, Namespace: testNS,
			Labels:      map[string]string{stabilizedLabel: "true"},
			Annotations: map[string]string{"synology.csi.io/stabilization-cr": testVS},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: q(request)}},
			VolumeName:       testPV,
			StorageClassName: &sc,
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	if capacity != "" {
		p.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: q(capacity)}
	}
	return p
}

func csiPV(capacity string, phase corev1.PersistentVolumePhase, claimed bool) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: testPV},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:         corev1.ResourceList{corev1.ResourceStorage: q(capacity)},
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: testClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           "csi.san.synology.com",
					VolumeHandle:     "handle",
					VolumeAttributes: map[string]string{"protocol": "nfs", "dsm": "192.168.0.200", "baseDir": "/volume1/k8s-csi-pvc-x"},
				},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: phase},
	}
	if claimed {
		pv.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: testPVC, Namespace: testNS}
	}
	return pv
}

type harness struct {
	t   *testing.T
	r   *VolumeStabilizationReconciler
	c   client.Client
	rec *record.FakeRecorder
}

func newHarness(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) *harness {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&synologyv1.VolumeStabilization{}, &corev1.PersistentVolumeClaim{}, &corev1.PersistentVolume{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	rec := record.NewFakeRecorder(64)
	return &harness{t: t, c: c, rec: rec, r: &VolumeStabilizationReconciler{Client: c, Scheme: testScheme(t), Recorder: rec}}
}

func (h *harness) reconcile() ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: testVS, Namespace: testNS}})
	if err != nil {
		h.t.Fatalf("Reconcile returned error: %v", err)
	}
	return res
}

func (h *harness) vs() *synologyv1.VolumeStabilization {
	h.t.Helper()
	v := &synologyv1.VolumeStabilization{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: testVS, Namespace: testNS}, v); err != nil {
		h.t.Fatalf("get VS: %v", err)
	}
	return v
}

func (h *harness) pvc() *corev1.PersistentVolumeClaim {
	h.t.Helper()
	p := &corev1.PersistentVolumeClaim{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: testPVC, Namespace: testNS}, p); err != nil {
		h.t.Fatalf("get PVC: %v", err)
	}
	return p
}

func (h *harness) setVSSize(size *resource.Quantity) {
	h.t.Helper()
	v := h.vs()
	v.Spec.Target.Size = size
	if err := h.c.Update(context.Background(), v); err != nil {
		h.t.Fatalf("update VS spec: %v", err)
	}
}

// setCapacity plays the CSI resizer: it records the new actual capacity.
func (h *harness) setCapacity(capacity string) {
	h.t.Helper()
	p := h.pvc()
	p.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: q(capacity)}
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		h.t.Fatalf("set capacity: %v", err)
	}
}

func (h *harness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func cond(v *synologyv1.VolumeStabilization, t string) *synologyv1.VolumeStabilizationCondition {
	for i := range v.Status.Conditions {
		if v.Status.Conditions[i].Type == t {
			return &v.Status.Conditions[i]
		}
	}
	return nil
}

func wantQty(t *testing.T, what string, got resource.Quantity, want string) {
	t.Helper()
	w := q(want)
	if got.Cmp(w) != 0 {
		t.Fatalf("%s = %s, want %s", what, got.String(), want)
	}
}

func hasEvent(evs []string, substr string) bool {
	for _, e := range evs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------- resize path --

func TestResize_SizeUnsetLeavesPVCAlone(t *testing.T) {
	h := newHarness(t, nil, boundVS(nil), stablePVC("15Gi", "15Gi"))
	res := h.reconcile()

	wantQty(t, "PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "15Gi")
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("unexpected requeue %+v", res)
	}
	v := h.vs()
	if v.Status.CurrentSize != "15Gi" {
		t.Fatalf("currentSize = %q, want 15Gi", v.Status.CurrentSize)
	}
	if cond(v, conditionResized) != nil || cond(v, conditionResizeRejected) != nil {
		t.Fatalf("no resize conditions expected when size is unset: %+v", v.Status.Conditions)
	}
}

func TestResize_GrowPatchesPVC(t *testing.T) {
	h := newHarness(t, nil, boundVS(qp("30Gi")), stablePVC("15Gi", "15Gi"))
	res := h.reconcile()

	wantQty(t, "PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")
	if res.RequeueAfter != resizePollInterval {
		t.Fatalf("expected requeue after %s while the CSI driver works, got %+v", resizePollInterval, res)
	}
	v := h.vs()
	c := cond(v, conditionResized)
	if c == nil || c.Status != corev1.ConditionFalse || c.Reason != "Resizing" {
		t.Fatalf("Resized condition = %+v, want False/Resizing", c)
	}
	if v.Status.CurrentSize != "15Gi" {
		t.Fatalf("currentSize must stay the REAL capacity (15Gi) until the CSI driver finishes, got %q", v.Status.CurrentSize)
	}
	if !hasEvent(h.events(), "Resizing") {
		t.Fatal("expected a Resizing event")
	}
}

func TestResize_FullGrowCycleReachesResized(t *testing.T) {
	h := newHarness(t, nil, boundVS(nil), stablePVC("15Gi", "15Gi"))
	h.reconcile()

	h.setVSSize(qp("30Gi"))
	h.reconcile() // patches the PVC
	h.setCapacity("30Gi")
	res := h.reconcile() // observes completion

	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("no requeue expected once resized, got %+v", res)
	}
	v := h.vs()
	if c := cond(v, conditionResized); c == nil || c.Status != corev1.ConditionTrue {
		t.Fatalf("Resized = %+v, want True", c)
	}
	if v.Status.CurrentSize != "30Gi" {
		t.Fatalf("currentSize = %q, want 30Gi", v.Status.CurrentSize)
	}
}

func TestResize_WaitsWhileCSIHasNotFinished(t *testing.T) {
	// request already 30Gi, capacity still 15Gi.
	h := newHarness(t, nil, boundVS(qp("30Gi")), stablePVC("30Gi", "15Gi"))
	res := h.reconcile()

	if res.RequeueAfter != resizePollInterval {
		t.Fatalf("expected poll requeue, got %+v", res)
	}
	v := h.vs()
	if c := cond(v, conditionResized); c == nil || c.Status != corev1.ConditionFalse {
		t.Fatalf("Resized = %+v, want False while pending", c)
	}
	if v.Status.CurrentSize != "15Gi" {
		t.Fatalf("currentSize = %q, want 15Gi", v.Status.CurrentSize)
	}
	wantQty(t, "PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")
}

func TestResize_ShrinkIsRejectedAndPVCUntouched(t *testing.T) {
	h := newHarness(t, nil, boundVS(qp("10Gi")), stablePVC("15Gi", "15Gi"))
	h.reconcile()

	wantQty(t, "PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "15Gi")
	v := h.vs()
	c := cond(v, conditionResizeRejected)
	if c == nil || c.Status != corev1.ConditionTrue || c.Reason != "ShrinkNotSupported" {
		t.Fatalf("ResizeRejected = %+v, want True/ShrinkNotSupported", c)
	}
	if !strings.Contains(c.Message, "10Gi") || !strings.Contains(c.Message, "15Gi") {
		t.Fatalf("message should name both sizes: %q", c.Message)
	}
	if !hasEvent(h.events(), "ShrinkNotSupported") {
		t.Fatal("expected a Warning event")
	}
}

func TestResize_ZeroOrNegativeSizeRejected(t *testing.T) {
	for _, tc := range []string{"0", "-5Gi"} {
		t.Run(tc, func(t *testing.T) {
			h := newHarness(t, nil, boundVS(qp(tc)), stablePVC("15Gi", "15Gi"))
			h.reconcile()
			wantQty(t, "PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "15Gi")
			c := cond(h.vs(), conditionResizeRejected)
			if c == nil || c.Reason != "InvalidSize" {
				t.Fatalf("ResizeRejected = %+v, want InvalidSize", c)
			}
		})
	}
}

func TestResize_EquivalentQuantitiesAreNotAResize(t *testing.T) {
	// 15Gi == 15360Mi == 16106127360 bytes. Cmp-based comparison must not patch.
	for _, s := range []string{"15360Mi", "16106127360"} {
		t.Run(s, func(t *testing.T) {
			patched := false
			h := newHarness(t, &interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
					if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
						patched = true
					}
					return c.Patch(ctx, obj, p, o...)
				},
			}, boundVS(qp(s)), stablePVC("15Gi", "15Gi"))
			h.reconcile()
			if patched {
				t.Fatal("PVC was patched although the size is numerically equal")
			}
			if c := cond(h.vs(), conditionResized); c == nil || c.Status != corev1.ConditionTrue {
				t.Fatalf("Resized = %+v, want True", c)
			}
		})
	}
}

func TestResize_APIForbidsExpansionIsReportedNotRetried(t *testing.T) {
	// e.g. StorageClass without allowVolumeExpansion: the API server answers 403/422.
	forbid := apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, testPVC,
		&simpleErr{"only dynamically provisioned pvc can be resized and the storageclass that provisions the pvc must support resize"})
	h := newHarness(t, &interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				return forbid
			}
			return c.Patch(ctx, obj, p, o...)
		},
	}, boundVS(qp("30Gi")), stablePVC("15Gi", "15Gi"))

	res := h.reconcile() // must not return an error
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("a rejected expansion must not hot-loop, got %+v", res)
	}
	c := cond(h.vs(), conditionResizeRejected)
	if c == nil || c.Reason != "ExpansionRejected" || !strings.Contains(c.Message, "storageclass") {
		t.Fatalf("ResizeRejected = %+v", c)
	}
	// a second reconcile with no spec change must stay quiet
	before := h.vs().ResourceVersion
	h.reconcile()
	if after := h.vs().ResourceVersion; after != before {
		t.Fatalf("status written again without any change (rv %s -> %s)", before, after)
	}
}

func TestResize_ConflictOnPatchRequeues(t *testing.T) {
	h := newHarness(t, &interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				return apierrors.NewConflict(schema.GroupResource{Resource: "persistentvolumeclaims"}, testPVC, &simpleErr{"the object has been modified"})
			}
			return c.Patch(ctx, obj, p, o...)
		},
	}, boundVS(qp("30Gi")), stablePVC("15Gi", "15Gi"))
	res := h.reconcile()
	if !res.Requeue {
		t.Fatalf("conflict must requeue, got %+v", res)
	}
}

func TestResize_UnexpectedPatchErrorIsReturned(t *testing.T) {
	h := newHarness(t, &interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				return apierrors.NewInternalError(&simpleErr{"boom"})
			}
			return c.Patch(ctx, obj, p, o...)
		},
	}, boundVS(qp("30Gi")), stablePVC("15Gi", "15Gi"))
	_, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: testVS, Namespace: testNS}})
	if err == nil {
		t.Fatal("transient server errors must surface so controller-runtime retries with backoff")
	}
}

func TestResize_SteadyStateWritesNothing(t *testing.T) {
	h := newHarness(t, nil, boundVS(qp("30Gi")), stablePVC("30Gi", "30Gi"))
	h.reconcile() // first pass records currentSize + Resized
	rvVS := h.vs().ResourceVersion
	rvPVC := h.pvc().ResourceVersion
	for i := 0; i < 5; i++ {
		res := h.reconcile()
		if res.Requeue || res.RequeueAfter != 0 {
			t.Fatalf("steady state must not requeue: %+v", res)
		}
	}
	if got := h.vs().ResourceVersion; got != rvVS {
		t.Fatalf("VS rewritten in steady state (rv %s -> %s) — this would loop in a real cluster", rvVS, got)
	}
	if got := h.pvc().ResourceVersion; got != rvPVC {
		t.Fatalf("PVC rewritten in steady state (rv %s -> %s)", rvPVC, got)
	}
}

func TestResize_RejectedThenFixedClearsCondition(t *testing.T) {
	h := newHarness(t, nil, boundVS(qp("10Gi")), stablePVC("15Gi", "15Gi"))
	h.reconcile()
	if cond(h.vs(), conditionResizeRejected) == nil {
		t.Fatal("precondition: should be rejected")
	}
	h.setVSSize(qp("15Gi"))
	h.reconcile()
	v := h.vs()
	if cond(v, conditionResizeRejected) != nil {
		t.Fatalf("ResizeRejected should be cleared after fixing the spec: %+v", v.Status.Conditions)
	}
	if c := cond(v, conditionResized); c == nil || c.Status != corev1.ConditionTrue {
		t.Fatalf("Resized = %+v, want True", c)
	}
}

func TestResize_RemovingSizeClearsConditions(t *testing.T) {
	h := newHarness(t, nil, boundVS(qp("30Gi")), stablePVC("30Gi", "30Gi"))
	h.reconcile()
	h.setVSSize(nil)
	h.reconcile()
	v := h.vs()
	if cond(v, conditionResized) != nil || cond(v, conditionResizeRejected) != nil {
		t.Fatalf("conditions should be removed when size is unmanaged: %+v", v.Status.Conditions)
	}
	wantQty(t, "PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")
}

func TestResize_ShrinkWhileExpansionPendingIsRejected(t *testing.T) {
	// request 30Gi but capacity 15Gi (expansion pending); user asks for 20Gi.
	h := newHarness(t, nil, boundVS(qp("20Gi")), stablePVC("30Gi", "15Gi"))
	h.reconcile()
	wantQty(t, "PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")
	if c := cond(h.vs(), conditionResizeRejected); c == nil || c.Reason != "ShrinkNotSupported" {
		t.Fatalf("ResizeRejected = %+v", c)
	}
}

// The API server answers 422 for a claim that is not Bound yet. That is
// transient: do not patch, do not report a rejection, just wait.
func TestResize_UnboundPVCIsDeferredNotRejected(t *testing.T) {
	pvc := stablePVC("15Gi", "")
	pvc.Status.Phase = corev1.ClaimPending
	patched := false
	h := newHarness(t, &interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				patched = true
			}
			return c.Patch(ctx, obj, p, o...)
		},
	}, boundVS(qp("30Gi")), pvc)

	res := h.reconcile()
	if patched {
		t.Fatal("must not patch a claim that is not Bound")
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected a safety requeue, got %+v", res)
	}
	if cond(h.vs(), conditionResizeRejected) != nil {
		t.Fatal("a pending claim is not a rejection")
	}
}

// Validation errors embed a multi-line object diff. The condition message must be
// one short line, and identical across reconciles, or status flaps forever.
func TestResize_RejectionMessageIsOneStableLine(t *testing.T) {
	multi := apierrors.NewInvalid(schema.GroupKind{Kind: "PersistentVolumeClaim"}, testPVC, nil)
	multi.ErrStatus.Message = "PersistentVolumeClaim \"x\" is invalid: spec: Forbidden\n  core.PersistentVolumeClaimSpec{\n- Requests: 1Gi\n+ Requests: 2Gi\n  }"
	h := newHarness(t, &interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				return multi
			}
			return c.Patch(ctx, obj, p, o...)
		},
	}, boundVS(qp("30Gi")), stablePVC("15Gi", "15Gi"))

	h.reconcile()
	c := cond(h.vs(), conditionResizeRejected)
	if c == nil {
		t.Fatal("expected ResizeRejected")
	}
	if strings.ContainsAny(c.Message, "\n") {
		t.Fatalf("message must be a single line: %q", c.Message)
	}
	rv := h.vs().ResourceVersion
	h.reconcile()
	if got := h.vs().ResourceVersion; got != rv {
		t.Fatalf("status rewritten for an unchanged rejection (%s -> %s)", rv, got)
	}
}

func TestConciseAPIError(t *testing.T) {
	if got := conciseAPIError(&simpleErr{"line1\nline2"}); got != "line1" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("x", 500)
	if got := conciseAPIError(&simpleErr{long}); len([]rune(got)) > 241 {
		t.Fatalf("not capped: %d", len(got))
	}
}

func TestResize_OtherPVCFieldsPreserved(t *testing.T) {
	h := newHarness(t, nil, boundVS(qp("30Gi")), stablePVC("15Gi", "15Gi"))
	h.reconcile()
	p := h.pvc()
	if p.Spec.VolumeName != testPV {
		t.Fatalf("volumeName changed: %q", p.Spec.VolumeName)
	}
	if p.Spec.StorageClassName == nil || *p.Spec.StorageClassName != testClass {
		t.Fatalf("storageClassName changed: %v", p.Spec.StorageClassName)
	}
	if p.Labels[stabilizedLabel] != "true" || p.Annotations["synology.csi.io/stabilization-cr"] != testVS {
		t.Fatalf("operator labels/annotations lost: %v %v", p.Labels, p.Annotations)
	}
}

// -------------------------------------------------- heal / create (the bug) --

// driveHealToCreated deletes the stable PVC, then runs the reconciler through
// Bound -> Creating -> Bound, simulating the PV becoming Available.
func driveHealToCreated(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	if err := h.c.Delete(ctx, h.pvc()); err != nil {
		t.Fatal(err)
	}
	h.reconcile() // Bound -> Creating (clears claimRef)
	if got := h.vs().Status.Phase; got != synologyv1.VolumeStabilizationPhaseCreating {
		t.Fatalf("phase = %s, want Creating", got)
	}
	pv := &corev1.PersistentVolume{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: testPV}, pv); err != nil {
		t.Fatal(err)
	}
	pv.Status.Phase = corev1.VolumeAvailable
	if err := h.c.Status().Update(ctx, pv); err != nil {
		t.Fatal(err)
	}
	h.reconcile() // Creating -> Bound (creates PVC)
	if got := h.vs().Status.Phase; got != synologyv1.VolumeStabilizationPhaseBound {
		t.Fatalf("phase = %s, want Bound", got)
	}
}

// Regression for the stale-size bug: after an expansion status.originalPV.storage
// (15Gi) is out of date; heal must NOT recreate the PVC at 15Gi.
func TestHeal_RecreatesPVCAtLivePVCapacityWhenSizeUnset(t *testing.T) {
	h := newHarness(t, nil, boundVS(nil), stablePVC("30Gi", "30Gi"), csiPV("30Gi", corev1.VolumeReleased, true))
	driveHealToCreated(t, h)
	wantQty(t, "recreated PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")
}

func TestHeal_RecreatesPVCAtSpecSize(t *testing.T) {
	h := newHarness(t, nil, boundVS(qp("30Gi")), stablePVC("30Gi", "30Gi"), csiPV("30Gi", corev1.VolumeReleased, true))
	driveHealToCreated(t, h)
	wantQty(t, "recreated PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")
}

func TestHeal_NeverRequestsLessThanPVCapacity(t *testing.T) {
	// spec says 20Gi but the PV is already 30Gi: a PVC must not undercut its PV.
	h := newHarness(t, nil, boundVS(qp("20Gi")), stablePVC("30Gi", "30Gi"), csiPV("30Gi", corev1.VolumeReleased, true))
	driveHealToCreated(t, h)
	wantQty(t, "recreated PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")
}

// A PVC requesting MORE than its PV can never bind, and a volume can only be
// expanded after it is bound. So even when spec.target.size is larger than the
// PV, the PVC is created at the PV capacity and grown in a second step.
func TestCreate_NeverRequestsMoreThanPVCapacity(t *testing.T) {
	h := newHarness(t, nil, boundVS(qp("40Gi")), stablePVC("30Gi", "30Gi"), csiPV("30Gi", corev1.VolumeReleased, true))
	driveHealToCreated(t, h)
	wantQty(t, "recreated PVC request (must be bindable)", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")

	// while unbound nothing may happen (the API server would answer 422)
	h.reconcile()
	wantQty(t, "PVC request while still Pending", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")

	// the PV controller binds it; now the resize path takes it to the desired size
	p := h.pvc()
	p.Status.Phase = corev1.ClaimBound
	p.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: q("30Gi")}
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	wantQty(t, "PVC request after resize step", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "40Gi")
}

func TestSizeForNewPVC(t *testing.T) {
	cases := []struct {
		name       string
		pvCap      string
		origStored string
		spec       *resource.Quantity
		want       string
	}{
		{"pv wins over stale status", "30Gi", "15Gi", nil, "30Gi"},
		{"spec larger is NOT used at create (unbindable)", "15Gi", "15Gi", qp("30Gi"), "15Gi"},
		{"spec smaller ignored", "30Gi", "15Gi", qp("20Gi"), "30Gi"},
		{"pv capacity missing falls back to status", "", "15Gi", nil, "15Gi"},
		{"everything missing falls back to 1Gi", "", "", nil, "1Gi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs := boundVS(tc.spec)
			vs.Status.OriginalPV.Storage = tc.origStored
			pv := csiPV("1Gi", corev1.VolumeAvailable, false)
			if tc.pvCap == "" {
				pv.Spec.Capacity = nil
			} else {
				pv.Spec.Capacity = corev1.ResourceList{corev1.ResourceStorage: q(tc.pvCap)}
			}
			wantQty(t, "size", sizeForNewPVC(vs, pv), tc.want)
		})
	}
}

// ------------------------------------------------------ full state machine --

// simulateCluster plays the roles of kube-controller-manager + CSI between
// reconciles so the whole Pending -> Bound journey can run against a fake client.
func simulateCluster(t *testing.T, h *harness, pvCapacity string) {
	t.Helper()
	ctx := context.Background()
	pv := &corev1.PersistentVolume{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: testPV}, pv); err != nil {
		t.Fatal(err)
	}
	src := &corev1.PersistentVolumeClaim{}
	srcErr := h.c.Get(ctx, types.NamespacedName{Name: "pg-dynamic-0", Namespace: testNS}, src)

	switch {
	case apierrors.IsNotFound(srcErr) && pv.Status.Phase == corev1.VolumeBound && pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Name == "pg-dynamic-0":
		pv.Status.Phase = corev1.VolumeReleased // source PVC deleted -> PV released
		_ = h.c.Status().Update(ctx, pv)
	case pv.Spec.ClaimRef == nil && pv.Status.Phase == corev1.VolumeReleased:
		pv.Status.Phase = corev1.VolumeAvailable // claimRef cleared by operator
		_ = h.c.Status().Update(ctx, pv)
	}
	// bind the stable PVC once it exists
	st := &corev1.PersistentVolumeClaim{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: testPVC, Namespace: testNS}, st); err == nil && st.Status.Phase != corev1.ClaimBound {
		st.Status.Phase = corev1.ClaimBound
		st.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: q(pvCapacity)}
		_ = h.c.Status().Update(ctx, st)
		pv.Status.Phase = corev1.VolumeBound
		pv.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: testPVC, Namespace: testNS}
		_ = h.c.Update(ctx, pv)
	}
}

func TestLifecycle_FromPendingToResized(t *testing.T) {
	vs := &synologyv1.VolumeStabilization{
		ObjectMeta: metav1.ObjectMeta{Name: testVS, Namespace: testNS, Generation: 1},
		Spec: synologyv1.VolumeStabilizationSpec{
			SourcePVC: synologyv1.SourcePVCReference{Name: "pg-dynamic-0"},
			Target:    synologyv1.TargetVolumeSpec{PVCName: testPVC},
		},
	}
	sc := testClass
	src := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-dynamic-0", Namespace: testNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: testPV, StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: q("15Gi")}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := csiPV("15Gi", corev1.VolumeBound, false)
	pv.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "pg-dynamic-0", Namespace: testNS}
	h := newHarness(t, nil, vs, src, pv)

	// 1. stabilize (no size configured)
	for i := 0; i < 40 && h.vs().Status.Phase != synologyv1.VolumeStabilizationPhaseBound; i++ {
		h.reconcile()
		simulateCluster(t, h, "15Gi")
	}
	if got := h.vs().Status.Phase; got != synologyv1.VolumeStabilizationPhaseBound {
		t.Fatalf("never reached Bound, stuck in %s: %s", got, h.vs().Status.Message)
	}
	wantQty(t, "stable PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "15Gi")
	h.reconcile()
	if h.vs().Status.CurrentSize != "15Gi" {
		t.Fatalf("currentSize = %q", h.vs().Status.CurrentSize)
	}

	// 2. user sets spec.target.size: 30Gi and applies
	h.setVSSize(qp("30Gi"))
	h.reconcile()
	wantQty(t, "stable PVC request after resize", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")

	// 3. CSI finishes
	h.setCapacity("30Gi")
	h.reconcile()
	v := h.vs()
	if c := cond(v, conditionResized); c == nil || c.Status != corev1.ConditionTrue {
		t.Fatalf("Resized = %+v", c)
	}
	if v.Status.CurrentSize != "30Gi" {
		t.Fatalf("currentSize = %q", v.Status.CurrentSize)
	}

	// 4. PV capacity follows (what the real resizer does), PVC is then deleted
	//    by accident: healed volume must come back at 30Gi, not 15Gi.
	pvObj := &corev1.PersistentVolume{}
	_ = h.c.Get(context.Background(), types.NamespacedName{Name: testPV}, pvObj)
	pvObj.Spec.Capacity = corev1.ResourceList{corev1.ResourceStorage: q("30Gi")}
	pvObj.Status.Phase = corev1.VolumeReleased
	if err := h.c.Update(context.Background(), pvObj); err != nil {
		t.Fatal(err)
	}
	_ = h.c.Status().Update(context.Background(), pvObj)
	driveHealToCreated(t, h)
	wantQty(t, "healed PVC request", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")
}

// size configured from day one, larger than the PV: stabilization must still
// produce a bindable PVC (at the PV capacity) and only then grow it.
func TestLifecycle_SizeSetFromDayOne(t *testing.T) {
	vs := &synologyv1.VolumeStabilization{
		ObjectMeta: metav1.ObjectMeta{Name: testVS, Namespace: testNS, Generation: 1},
		Spec: synologyv1.VolumeStabilizationSpec{
			SourcePVC: synologyv1.SourcePVCReference{Name: "pg-dynamic-0"},
			Target:    synologyv1.TargetVolumeSpec{PVCName: testPVC, Size: qp("30Gi")},
		},
	}
	sc := testClass
	src := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-dynamic-0", Namespace: testNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: testPV, StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: q("15Gi")}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := csiPV("15Gi", corev1.VolumeBound, false)
	pv.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "pg-dynamic-0", Namespace: testNS}
	h := newHarness(t, nil, vs, src, pv)

	for i := 0; i < 40 && h.vs().Status.Phase != synologyv1.VolumeStabilizationPhaseBound; i++ {
		h.reconcile()
		simulateCluster(t, h, "15Gi")
	}
	if got := h.vs().Status.Phase; got != synologyv1.VolumeStabilizationPhaseBound {
		t.Fatalf("never reached Bound, stuck in %s: %s", got, h.vs().Status.Message)
	}
	wantQty(t, "PVC at creation must be bindable (PV capacity)", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "15Gi")

	h.reconcile() // bound -> resize step
	wantQty(t, "PVC after resize step", h.pvc().Spec.Resources.Requests[corev1.ResourceStorage], "30Gi")
}

// ------------------------------------------------------------ cond helpers --

func TestUpsertCondition(t *testing.T) {
	vs := &synologyv1.VolumeStabilization{}
	if !upsertCondition(vs, "X", corev1.ConditionFalse, "R", "m") {
		t.Fatal("first insert must report change")
	}
	t0 := vs.Status.Conditions[0].LastTransitionTime.Time
	time.Sleep(5 * time.Millisecond)

	if upsertCondition(vs, "X", corev1.ConditionFalse, "R", "m") {
		t.Fatal("identical upsert must report no change")
	}
	if !vs.Status.Conditions[0].LastTransitionTime.Time.Equal(t0) {
		t.Fatal("unchanged upsert must not re-stamp the transition time")
	}

	if !upsertCondition(vs, "X", corev1.ConditionFalse, "R", "other message") {
		t.Fatal("message change must report change")
	}
	if !vs.Status.Conditions[0].LastTransitionTime.Time.Equal(t0) {
		t.Fatal("transition time must only move when status flips")
	}

	if !upsertCondition(vs, "X", corev1.ConditionTrue, "R", "other message") {
		t.Fatal("status flip must report change")
	}
	if !vs.Status.Conditions[0].LastTransitionTime.Time.After(t0) {
		t.Fatal("status flip must move the transition time")
	}
	if len(vs.Status.Conditions) != 1 {
		t.Fatalf("expected a single condition, got %d", len(vs.Status.Conditions))
	}
}

func TestRemoveCondition(t *testing.T) {
	vs := &synologyv1.VolumeStabilization{}
	upsertCondition(vs, "A", corev1.ConditionTrue, "r", "m")
	upsertCondition(vs, "B", corev1.ConditionTrue, "r", "m")
	if !removeCondition(vs, "A") || len(vs.Status.Conditions) != 1 || vs.Status.Conditions[0].Type != "B" {
		t.Fatalf("remove A failed: %+v", vs.Status.Conditions)
	}
	if removeCondition(vs, "A") {
		t.Fatal("removing a missing condition must report false")
	}
}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
