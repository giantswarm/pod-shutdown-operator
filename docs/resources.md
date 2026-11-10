# Resources

All resources are in the API group `shutdown.giantswarm.io/v1alpha1`.

## ManagedApplication

Namespaced.
Each application team writes one for each application.

| Field | Default | Description |
|---|---|---|
| `spec.workloads` | | The workloads of the application, in the namespace of the resource. Each item has a `kind` (`Deployment`, `StatefulSet`, `CronJob`, or `VirtualMachine`) and a `name`. |
| `spec.dependsOn` | | The applications that this application depends on. Use `name` for the same namespace, or `namespace/name`. |
| `spec.gracePeriod` | `30s` | The time that the application gets to stop. |
| `spec.readyTimeout` | `5m` | The time that the application gets to become ready at scale-up. |

Make the `terminationGracePeriodSeconds` of the pods equal to the `gracePeriod`.
A `ShutdownPlan` shows a warning for a difference.

## ShutdownRequest

Cluster-scoped.
The only valid name is `cluster`.
The HTTP endpoint writes it.
Refer to [How it works](how-it-works.md).

| Field | Default | Description |
|---|---|---|
| `spec.action` | | `Shutdown` or `Cancel`. |
| `spec.budget` | `10m` | The time from the start of the shutdown to the node power-off. |
| `spec.safetyMargin` | `30s` | The time at the end of the budget that the operator keeps free. |
| `status.phase` | | `ShuttingDown`, `Complete`, `ScalingUp`, or `Restored`. |
| `status.recordedState` | | The state of the workloads and the Flux controllers at the start of the shutdown. |
| `status.unmanaged` | | The running workloads that no `ManagedApplication` lists. |
| `status.failed` | | The applications that the operator could not stop. |
| `status.degraded` | | The applications that did not become ready at scale-up. |

## ShutdownPlan

Cluster-scoped.
A dry run of a shutdown.
The operator writes the plan once into the status and changes nothing in the cluster.
It deletes the `ShutdownPlan` after `shutdownPlanTTL` (default `1h`).

```sh
kubectl create -f - <<EOF
apiVersion: shutdown.giantswarm.io/v1alpha1
kind: ShutdownPlan
metadata:
  name: check
EOF
kubectl get shutdownplan check --output jsonpath='{.status.report}'
```

When a shutdown is active, the plan uses its record and budget.
Else it uses the live state and the default budget.

The plan uses the permissions of the operator.
A user who can create and read a `ShutdownPlan` sees the names of all running Deployments, StatefulSets, and VMs in the cluster.
`status.unmanaged` of the `ShutdownRequest` shows the same list.
Give these rights only to cluster administrators.

`status.result` is `OK`, `Warnings`, or `Errors`.
It is the worst severity of the findings. An `Info` finding does not change it.
Each item in `status.findings` has a `reason`:

| Reason | Severity | Cause |
|---|---|---|
| `Cycle` | Error | A dependency cycle. |
| `DepthAboveLimit` | Error | A dependency group with more than 3 tiers. |
| `UnknownDependency` | Error | A `dependsOn` name that is not in the plan. |
| `MissingWorkload` | Error | A listed workload that does not exist. |
| `OperatorWorkload` | Error | A `ManagedApplication` lists the operator Deployment. |
| `PatchRejected`, `PatchFailed` | Error | The API server rejected or failed a dry-run stop patch, for example because of RBAC or admission. |
| `UnreadableBootID` | Error | The operator cannot read the boot ID of its node. |
| `GracePeriodShrink` | Warning | The longest chain of a group does not fit in the budget. |
| `TerminationGracePeriodLonger`, `TerminationGracePeriodShorter` | Warning | The `terminationGracePeriodSeconds` of a workload is not equal to the `gracePeriod` of its application. |
| `UnmanagedWorkload` | Info | A running workload that no `ManagedApplication` lists. System workloads, for example in `kube-system`, are always in this list. |

Each finding has a `fallback` field.
It tells what a real shutdown does with the problem.
