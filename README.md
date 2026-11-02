# pod-shutdown-operator

A Kubernetes operator which scales down workloads when it receives a trigger via an API endpoint. Used for safely scaling workloads down before a cluster shutdown.

The operator stops the applications in dependency order inside a time budget. It records the replica counts at the start of a shutdown, and restores them when the cluster powers on. Each application team declares its application in a `ManagedApplication` resource.

## Install

The Helm chart is in `helm/pod-shutdown-operator`. It installs the CRDs, the operator Deployment, the RBAC, the token Secret, and a `NodePort` service for the HTTP endpoint.

- The operator runs on a control-plane node, with the PriorityClass `system-cluster-critical`.
- The operator never stops its own Deployment, also when a `ManagedApplication` lists it. Its only dependency in the cluster is the API server.
- Set `api.token`, or set `api.existingSecret` to the name of a Secret with the key `token`.

## Documents

- [Runbook](docs/runbook.md): the calls to the HTTP endpoint, and the manual command that scales the Flux controllers back.
- [Kubelet recommendation](docs/kubelet.md): the `shutdownGracePeriodByPodPriority` setting for the cluster build team.
- [Development](docs/development.md): code generation and tests.

## Tests

- `make test-envtest` runs the behavior tests against a local API server.
- `make test-e2e` runs the end-to-end tests in a `kind` cluster. The tests cover a full shutdown and scale-up, the operator killed in the middle of a shutdown, two operator pods with leader election, and the forced delete with the StatefulSet delay.

The automatic tests cannot cover all the behavior. A `kind` node shares the kernel of the host, so its boot ID does not change. `kind` also runs no KubeVirt. Do these manual tests on a lab node before a release:

1. **A real power pull.** Start a shutdown. Pull the power of all nodes in the middle of the shutdown. Restore the power. Make sure that the operator finds the new boot ID and scales the applications up from the record. Make sure that the `ShutdownRequest` reaches the phase `Restored`, and that the Flux controllers run again. Do the test again after a shutdown that reached `Complete`.
2. **A Windows VM and the `Halted` flow.** List a Windows VM in a `ManagedApplication`. Start a shutdown. Make sure that the operator sets `runStrategy: Halted`, and that the guest shuts down cleanly inside its `gracePeriod`. Do the test again with a short `gracePeriod`, and make sure that the operator stops the VM by force with the `stop` subresource at the deadline. Cancel the shutdown, and make sure that the VM gets its old `runStrategy` back and starts.
