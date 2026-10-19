# Developing on pod-shutdown-operator

## Generate code and manifests

After a change to the API types in `api/v1alpha1` or to an RBAC marker, run:

    make generate manifests

This command writes `zz_generated.deepcopy.go`, the CRDs in `config/crd/bases`, and the ClusterRole in `config/rbac/role.yaml`. Commit the result. CI fails when these files are out of date.

## Run the tests

The behavior tests use `envtest`. `envtest` starts a real API server and etcd, but no other Kubernetes controller. The tests play the Deployment, StatefulSet, and kubelet controllers: they create and delete Pods, and set the workload status, by hand.

    make test-envtest

The target downloads the tools and the envtest binaries into `bin/`. The test files have the build tag `envtest`, so `make test` does not run them.
