# pod-shutdown-operator

A Kubernetes operator which scales down workloads when it receives a trigger via an API endpoint.
Used for safely scaling workloads down before a cluster shutdown.

- The operator stops applications in dependency order: an application stops after the applications that depend on it.
- All stops fit in a time budget before the node power-off.
- It records the state of each workload at the start of a shutdown, and restores that state when the cluster powers on again.
- It supports Deployments, StatefulSets, CronJobs, and KubeVirt VirtualMachines.

Each application team declares its application in a `ManagedApplication` resource:

```yaml
apiVersion: shutdown.giantswarm.io/v1alpha1
kind: ManagedApplication
metadata:
  name: shop
  namespace: shop
spec:
  workloads:
    - kind: Deployment
      name: shop-frontend
    - kind: CronJob
      name: shop-report
  # The operator stops shop before the applications in this list.
  dependsOn:
    - databases/postgres
  gracePeriod: 60s
  readyTimeout: 5m
```

## Documents

Start with [Getting started](docs/getting-started.md).
Refer to [docs](docs/README.md) for all documents.
