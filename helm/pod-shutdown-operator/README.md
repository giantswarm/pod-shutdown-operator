# pod-shutdown-operator

A Kubernetes operator that stops applications in dependency order before a cluster shutdown, and restores them at power-on.

**Homepage:** <https://gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator>

## Source Code

* <https://gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| image.name | string | `"giantswarm/pod-shutdown-operator"` |  |
| image.tag | string | `""` | The image tag. Empty means the appVersion of the chart. |
| registry.domain | string | `"gsoci.azurecr.io"` |  |
| replicas | int | `1` | The number of operator pods. Only the holder of the leader election lease acts. |
| api.existingSecret | string | `""` | The name of an existing Secret that holds the token of the HTTP endpoint in the key `token`. Empty means that the chart creates the Secret. |
| api.token | string | `""` | The token of the HTTP endpoint. The chart uses it only when `api.existingSecret` is empty. Use at least 32 random bytes, for example from `openssl rand -base64 32`. Empty means that the chart keeps the token of the existing Secret, or generates a token from 32 random bytes. |
| api.nodePort | int | `30082` | The node port of the HTTP endpoint. The callers send their calls to this port on a node. |
| operator.fluxControllers | list | `["flux-system/kustomize-controller","flux-system/helm-controller"]` | The Flux controller Deployments, as namespace/name. The operator scales them to 0 before it stops an application. An empty list disables the Flux step. |
| operator.fluxStopTimeout | string | `"30s"` | The longest time that the shutdown waits for the pods of the Flux controllers to end. |
| operator.scaleUpTimeout | string | `"30m"` | The global time limit of the scale-up. |
| operator.statefulSetScaleUpDelay | string | `"5s"` | The time that a StatefulSet scale-up waits after a forced delete of its pods. |
| operator.staleLimit | string | `"5m"` | The age after which a request with no phase is stale. |
| operator.shutdownPlanTTL | string | `"1h"` | The age after which the operator deletes a ShutdownPlan. |
| operator.pollInterval | string | `"2s"` | The interval between checks while pods end and while workloads become ready. |
| priorityClassName | string | `"system-cluster-critical"` | The PriorityClass of the operator pods. A high priority keeps the operator until the node power-off. |
| nodeSelector | object | `{"node-role.kubernetes.io/control-plane":""}` | The operator runs on a control-plane node, so that it stays alive to the end of the shutdown. |
| tolerations | list | `[{"effect":"NoSchedule","key":"node-role.kubernetes.io/control-plane","operator":"Exists"}]` | The tolerations that let the operator run on a control-plane node. |
| resources.requests.cpu | string | `"50m"` |  |
| resources.requests.memory | string | `"64Mi"` |  |
| resources.limits.memory | string | `"256Mi"` |  |

## CI values

`ci/ci-values.yaml` holds the values the CI render (`helm template` in app-build-suite) needs beyond the defaults, such as a `required` value without a default.
