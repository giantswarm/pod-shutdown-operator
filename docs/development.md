# Developing on pod-shutdown-operator

## Generate code and manifests

After a change to the API types in `api/v1alpha1` or to an RBAC marker, run:

    make generate manifests

This command writes `zz_generated.deepcopy.go`, the CRDs in `helm/pod-shutdown-operator/files/crds`, and the ClusterRole in `helm/pod-shutdown-operator/files/rbac/role.yaml`.
The Helm chart reads these files.
Commit the result.
CI fails when these files are out of date.

## Run the tests

The behavior tests use `envtest`.
`envtest` starts a real API server and etcd, but no other Kubernetes controller.
The tests play the Deployment, StatefulSet, and kubelet controllers: they create and delete Pods, and set the workload status, by hand.

envtest also runs no KubeVirt.
The tests install minimal KubeVirt CRDs from `internal/operator/testdata/crd`, and they play KubeVirt by hand: they create and delete VirtualMachineInstances, and set the VM `Ready` condition. envtest cannot serve the `start` and `stop` subresources, so the tests give the operator a stub for them.
A manual test on a lab node covers a real Windows VM.

    make test-envtest

The target downloads the tools and the envtest binaries into `bin/`.
The test files have the build tag `envtest`, so `make test` does not run them.

## Run the end-to-end tests

The end-to-end tests run the operator image in a `kind` cluster with one control-plane node and one worker.
They use the Kubernetes API and the HTTP endpoint.
They play the Flux controllers with Deployments of their own.
The tests cover a full shutdown and scale-up, the operator killed in the middle of a shutdown, two operator pods with leader election, and the forced delete with the StatefulSet delay.

    make test-e2e

The target builds the binary and the image, creates the cluster, loads the image, and installs the chart with `tests/e2e/values.yaml`.
It needs `docker`, `helm`, and `kubectl`.
With podman, set two variables:

    CONTAINER_TOOL=podman KIND_EXPERIMENTAL_PROVIDER=podman make test-e2e

The cluster stays after the tests, so a new run is faster.
The kubeconfig is `bin/e2e/kubeconfig`.
Delete the cluster with `make e2e-delete`.

The test files have the build tag `e2e`, so `make test` and `make test-envtest` do not run them.

## Do the manual tests before a release

The automatic tests cannot cover all the behavior.
A `kind` node shares the kernel of the host, so its boot ID does not change.
`kind` also runs no KubeVirt.
Do these tests on a lab node:

1. **A real power pull.**
   Start a shutdown.
   Pull the power of all nodes in the middle of the shutdown.
   Restore the power.
   Make sure that the operator finds the new boot ID and scales the applications up from the record.
   Make sure that the `ShutdownRequest` reaches the phase `Restored`, and that the Flux controllers run again.
   Do the test again after a shutdown that reached `Complete`.
2. **A Windows VM and the `Halted` flow.**
   List a Windows VM in a `ManagedApplication`.
   Start a shutdown.
   Make sure that the operator sets `runStrategy: Halted`, and that the guest shuts down cleanly inside its `gracePeriod`.
   Do the test again with a short `gracePeriod`, and make sure that the operator stops the VM by force with the `stop` subresource at the deadline.
   Cancel the shutdown, and make sure that the VM gets its old `runStrategy` back and starts.
