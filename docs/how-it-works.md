# How it works

The operator keeps the state of a shutdown in the `ShutdownRequest` with the name `cluster`. A call to `POST /shutdown` creates it with the action `Shutdown`. A call to `POST /cancel` sets the action `Cancel`. Refer to [HTTP API](http-api.md).

```
ShuttingDown ──> Complete
     │              │
     └── Cancel ────┴──> ScalingUp ──> Restored
```

A `Cancel` in `ShuttingDown` or `Complete` starts the scale-up. A `Shutdown` in `ScalingUp` returns to `ShuttingDown` with the same record. A `Shutdown` in `Restored` starts a new shutdown.

## Shutdown

1. The operator writes the record in one status update: the state of each listed workload, the Flux replica counts, the boot ID of each node, and the start time. It changes no workload before the record exists.
2. It scales the Flux controllers to 0, so that Flux cannot revert the scale-down. It waits up to `fluxStopTimeout` for the Flux pods to end.
3. It stops the applications. An application stops after all the applications that depend on it. The applications of a dependency cycle stop as one step.
4. At the forced stop time, it deletes the remaining pods with a grace period of 0.
5. When each application has stopped or failed, it sets the phase `Complete`. The field `status.failed` lists the applications that it could not stop.

The action on each workload kind:

| Kind | Action |
|---|---|
| Deployment, StatefulSet | Scale to 0 replicas. |
| CronJob | Set `suspend: true`. Jobs that run do not stop. |
| VirtualMachine | Set `runStrategy: Halted`, or `running: false`. KubeVirt then shuts down the guest. The forced stop uses the `stop` subresource. |

The operator also lists the running workloads that no `ManagedApplication` lists in `status.unmanaged`. It does not stop them. Refer to the [kubelet recommendation](kubelet.md) for these pods.

## Time budget

The budget is the time from the start of the shutdown to the node power-off. The default is `10m`, with a safety margin of `30s`. To set other values, create the `ShutdownRequest` with `kubectl` and not through the HTTP endpoint.

- A dependency group is a set of applications that `dependsOn` links together. The groups stop in parallel.
- Each group gets the time from the end of the Flux step to the end of the budget minus the safety margin.
- When the `gracePeriod` sum of the longest chain in a group is longer than that time, each `gracePeriod` in the group shrinks by the same ratio.
- Each application has a deadline that leaves time for the chain below it. The forced stop comes at the earlier of the deadline and the start of the stop plus the `gracePeriod`.

Use a [`ShutdownPlan`](resources.md#shutdownplan) to see the time plan before a real shutdown.

## Scale-up

1. The operator sets the phase `ScalingUp`.
2. It restores each workload from the record. An application starts when all its dependencies are ready or degraded.
3. An application that is not ready in its `readyTimeout` goes into `status.degraded`.
4. It scales the Flux controllers back last, and sets the phase `Restored`.

`scaleUpTimeout` (default `30m`) is a global limit, and it overrides `readyTimeout`. At the limit, the operator starts all remaining applications at once, with no wait for their dependencies. Each application that is not ready at that time goes into `status.degraded`, also when it becomes ready some seconds later. With the defaults, a chain of six applications that each use their full `readyTimeout` reaches the limit.

A restored CronJob without `startingDeadlineSeconds` starts its missed Job at once. To prevent this, set `startingDeadlineSeconds` on the CronJob.

## Power cycle check

When the operator starts, the leader compares the boot ID of its node with `status.bootIDs`. A different boot ID means that the cluster had a power cycle after the shutdown. The operator then sets the action `Cancel`, and the scale-up starts with no call from outside.

The operator also does this when the record has no entry for its node. Refer to the [runbook](runbook.md#start-a-shutdown-again-after-a-false-power-cycle).
