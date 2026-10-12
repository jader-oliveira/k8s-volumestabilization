export default function VolumeStabilizationArchitectureBoard() {
  const stepStyle = "rounded-2xl border border-slate-200 bg-white p-4 shadow-sm";
  const cardStyle = "rounded-3xl border border-slate-200 bg-white/95 shadow-lg";

  return (
    <div className="min-h-screen bg-slate-950 text-slate-900 p-8">
      <div className="mx-auto max-w-7xl space-y-8">
        <header className="rounded-[28px] border border-slate-800 bg-gradient-to-br from-slate-900 via-slate-900 to-blue-950 p-8 text-white shadow-2xl">
          <div className="flex flex-col gap-4 lg:flex-row lg:items-end lg:justify-between">
            <div className="max-w-3xl">
              <div className="mb-3 inline-flex items-center rounded-full border border-cyan-400/30 bg-cyan-400/10 px-3 py-1 text-xs font-medium tracking-wide text-cyan-200">
                Kubernetes Storage Control Plane
              </div>
              <h1 className="text-4xl font-semibold tracking-tight">Volume Stabilization Operator</h1>
              <p className="mt-3 text-base leading-7 text-slate-300">
                Non-destructive PVC stabilization for NFS-backed Kubernetes volumes. The operator converts a dynamic,
                UUID-named claim into a stable, predictable claim name without recreating the PV or touching NAS data.
              </p>
            </div>
            <div className="grid grid-cols-1 gap-3 text-sm lg:w-[360px]">
              <div className="rounded-2xl border border-emerald-400/20 bg-emerald-400/10 p-4 text-emerald-100">
                <div className="text-xs uppercase tracking-[0.2em] text-emerald-300">Guarantee</div>
                <div className="mt-1 text-lg font-semibold">Original PV is never deleted</div>
              </div>
              <div className="rounded-2xl border border-blue-400/20 bg-blue-400/10 p-4 text-blue-100">
                <div className="text-xs uppercase tracking-[0.2em] text-blue-300">Outcome</div>
                <div className="mt-1 text-lg font-semibold">Stable PVC name + same NAS export</div>
              </div>
            </div>
          </div>
        </header>

        <section className="grid gap-6 lg:grid-cols-[1.15fr_0.85fr]">
          <div className={`${cardStyle} p-6`}>
            <div className="mb-5 flex items-center justify-between">
              <div>
                <h2 className="text-2xl font-semibold text-slate-900">Target Architecture</h2>
                <p className="mt-1 text-sm text-slate-500">Clear separation between control plane, workload plane, and external storage.</p>
              </div>
              <div className="rounded-full bg-slate-100 px-3 py-1 text-xs font-medium text-slate-600">Steady State</div>
            </div>

            <div className="grid gap-5 xl:grid-cols-[1fr_1fr_0.8fr]">
              <div className="rounded-3xl border border-blue-200 bg-blue-50 p-5">
                <div className="mb-4 flex items-center gap-3">
                  <div className="flex h-11 w-11 items-center justify-center rounded-2xl bg-blue-600 text-lg text-white">⚙️</div>
                  <div>
                    <div className="text-xs uppercase tracking-[0.18em] text-blue-700">Operator Namespace</div>
                    <div className="text-lg font-semibold text-slate-900">volume-stabilization</div>
                  </div>
                </div>
                <div className="space-y-3">
                  <div className="rounded-2xl border border-blue-200 bg-white p-4">
                    <div className="text-sm font-semibold">volume-stabilization-operator</div>
                    <div className="mt-1 text-sm text-slate-600">Deployment running the controller-runtime manager</div>
                  </div>
                  <div className="rounded-2xl border border-blue-200 bg-white p-4">
                    <div className="text-sm font-semibold">VolumeStabilizationReconciler</div>
                    <div className="mt-1 text-sm text-slate-600">Watches VolumeStabilization CRs and PVC events mapped by annotation</div>
                  </div>
                </div>
              </div>

              <div className="rounded-3xl border border-violet-200 bg-violet-50 p-5">
                <div className="mb-4 flex items-center gap-3">
                  <div className="flex h-11 w-11 items-center justify-center rounded-2xl bg-violet-600 text-lg text-white">⎈</div>
                  <div>
                    <div className="text-xs uppercase tracking-[0.18em] text-violet-700">Application Namespace</div>
                    <div className="text-lg font-semibold text-slate-900">default / workload namespace</div>
                  </div>
                </div>
                <div className="space-y-3">
                  <div className="rounded-2xl border border-violet-200 bg-white p-4">
                    <div className="text-sm font-semibold">VolumeStabilization CR</div>
                    <div className="mt-1 text-sm text-slate-600">Declares source PVC and target stable PVC</div>
                  </div>
                  <div className="grid gap-3 md:grid-cols-2">
                    <div className="rounded-2xl border border-amber-200 bg-amber-50 p-4">
                      <div className="text-sm font-semibold text-slate-900">Stable PVC</div>
                      <div className="mt-1 text-sm text-slate-600">my-app-data</div>
                      <div className="mt-2 inline-flex rounded-full bg-emerald-100 px-2.5 py-1 text-xs font-medium text-emerald-700">Bound</div>
                    </div>
                    <div className="rounded-2xl border border-slate-200 bg-slate-50 p-4">
                      <div className="text-sm font-semibold text-slate-900">Original PV</div>
                      <div className="mt-1 text-sm text-slate-600">Reused, not recreated</div>
                      <div className="mt-2 inline-flex rounded-full bg-blue-100 px-2.5 py-1 text-xs font-medium text-blue-700">Static re-bind</div>
                    </div>
                  </div>
                  <div className="rounded-2xl border border-violet-200 bg-white p-4">
                    <div className="text-sm font-semibold">Workload Pod</div>
                    <div className="mt-1 text-sm text-slate-600">Deployment / StatefulSet always mounts the stable claim name</div>
                  </div>
                </div>
              </div>

              <div className="rounded-3xl border border-emerald-200 bg-emerald-50 p-5">
                <div className="mb-4 flex items-center gap-3">
                  <div className="flex h-11 w-11 items-center justify-center rounded-2xl bg-emerald-600 text-lg text-white">🗄️</div>
                  <div>
                    <div className="text-xs uppercase tracking-[0.18em] text-emerald-700">External Storage</div>
                    <div className="text-lg font-semibold text-slate-900">Synology NAS</div>
                  </div>
                </div>
                <div className="rounded-2xl border border-emerald-200 bg-white p-4">
                  <div className="text-sm font-semibold">NFS Export</div>
                  <div className="mt-1 text-sm text-slate-600">/volume1/k8s-csi-pvc-&lt;uuid&gt;</div>
                  <div className="mt-3 rounded-2xl bg-emerald-100 p-3 text-sm text-emerald-800">
                    Same export. Same path. Same data.
                  </div>
                </div>
              </div>
            </div>

            <div className="mt-6 grid gap-3 md:grid-cols-4">
              <div className="rounded-2xl border border-slate-200 bg-slate-50 p-4 text-sm text-slate-700">CR declares stabilization intent</div>
              <div className="rounded-2xl border border-slate-200 bg-slate-50 p-4 text-sm text-slate-700">Operator reconciles and captures PV metadata</div>
              <div className="rounded-2xl border border-slate-200 bg-slate-50 p-4 text-sm text-slate-700">Stable PVC rebinds to original PV</div>
              <div className="rounded-2xl border border-slate-200 bg-slate-50 p-4 text-sm text-slate-700">Workload always targets stable claim name</div>
            </div>
          </div>

          <div className="space-y-6">
            <div className={`${cardStyle} p-6`}>
              <h3 className="text-xl font-semibold text-slate-900">Critical Safety Rules</h3>
              <div className="mt-4 space-y-3">
                <div className="rounded-2xl border border-rose-200 bg-rose-50 p-4">
                  <div className="text-sm font-semibold text-rose-700">Never delete the original PV</div>
                  <p className="mt-1 text-sm text-slate-700">Deleting the PV can remove the NAS export in the Synology CSI scenario.</p>
                </div>
                <div className="rounded-2xl border border-blue-200 bg-blue-50 p-4">
                  <div className="text-sm font-semibold text-blue-700">Delete only the source PVC</div>
                  <p className="mt-1 text-sm text-slate-700">The operator waits for the PV to transition and then patches it in place.</p>
                </div>
                <div className="rounded-2xl border border-emerald-200 bg-emerald-50 p-4">
                  <div className="text-sm font-semibold text-emerald-700">Static rebind preserves data</div>
                  <p className="mt-1 text-sm text-slate-700">The stable PVC sets volumeName to the original PV and bypasses dynamic provisioning.</p>
                </div>
              </div>
            </div>

            <div className={`${cardStyle} p-6`}>
              <h3 className="text-xl font-semibold text-slate-900">Self-Healing</h3>
              <div className="mt-4 space-y-3 text-sm text-slate-700">
                <div className="rounded-2xl bg-slate-50 p-4">PVC watch event fires when the stable PVC is deleted</div>
                <div className="rounded-2xl bg-slate-50 p-4">Reconciler clears stale claimRef if needed</div>
                <div className="rounded-2xl bg-slate-50 p-4">Stable PVC is recreated and rebound to the same PV</div>
              </div>
            </div>
          </div>
        </section>

        <section className={`${cardStyle} p-6`}>
          <div className="mb-5 flex items-center justify-between">
            <div>
              <h2 className="text-2xl font-semibold text-slate-900">Non-Destructive Stabilization Flow</h2>
              <p className="mt-1 text-sm text-slate-500">The sequence below reflects the actual documented control flow.</p>
            </div>
            <div className="rounded-full bg-slate-100 px-3 py-1 text-xs font-medium text-slate-600">Correct Order</div>
          </div>

          <div className="grid gap-4 lg:grid-cols-6">
            {[
              ["1", "Dynamic Provisioning", "Source PVC is created and bound to the original PV."],
              ["2", "Capture", "Reconciler reads PV server, path, capacity, access modes, and mount options."],
              ["3", "Delete Source PVC", "Only the original PVC is deleted. The PV is not removed."],
              ["4", "Patch PV", "claimRef and storageClassName are cleared in place so the PV becomes reusable."],
              ["5", "Create Stable PVC", "New predictable PVC is created with volumeName set to the original PV and storageClassName set to empty string."],
              ["6", "Immediate Rebind", "Kubernetes binds the stable PVC to the same PV and the workload mounts the same NAS export."]
            ].map(([n, title, text]) => (
              <div key={n} className={stepStyle}>
                <div className="mb-3 flex h-10 w-10 items-center justify-center rounded-2xl bg-slate-900 text-sm font-semibold text-white">{n}</div>
                <div className="text-sm font-semibold text-slate-900">{title}</div>
                <div className="mt-2 text-sm leading-6 text-slate-600">{text}</div>
              </div>
            ))}
          </div>

          <div className="mt-5 rounded-3xl border border-amber-200 bg-amber-50 p-5">
            <div className="text-sm font-semibold text-amber-800">Protected invariant</div>
            <div className="mt-1 text-sm text-slate-700">The NAS export and underlying data path remain unchanged throughout the entire process.</div>
          </div>
        </section>
      </div>
    </div>
  );
}
