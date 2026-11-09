# Install

The Helm chart is in `helm/pod-shutdown-operator`.
It installs the CRDs, the operator Deployment, the RBAC, the token Secret, and a `NodePort` service for the HTTP endpoint.

- The operator runs on a control-plane node, with the PriorityClass `system-cluster-critical`.
  It then stays alive to the end of the shutdown.
- The operator never stops its own Deployment, also when a `ManagedApplication` lists it.
  Its only dependency in the cluster is the API server.
- Set `api.token`, or set `api.existingSecret` to the name of a Secret with the key `token`.
- With `replicas` above 1, only the holder of the leader election Lease acts.

## Main values

| Value | Default | Description |
|---|---|---|
| `api.nodePort` | `30082` | The node port of the HTTP endpoint. |
| `operator.fluxControllers` | `flux-system/kustomize-controller`, `flux-system/helm-controller` | The Flux controllers that the operator scales to 0 first. An empty list disables the Flux step. |
| `operator.fluxStopTimeout` | `30s` | The longest time that the Flux step waits for the Flux pods to end. |
| `operator.scaleUpTimeout` | `30m` | The global time limit of the scale-up. |
| `operator.staleLimit` | `5m` | The age after which a request with no phase is stale. |
| `operator.shutdownPlanTTL` | `1h` | The age after which the operator deletes a `ShutdownPlan`. |

Refer to the [chart README](../helm/pod-shutdown-operator/README.md) for all values.
