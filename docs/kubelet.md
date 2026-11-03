# Kubelet recommendation for the cluster build team

The operator stops the applications through the Kubernetes API before the node power-off. Some pods can still run at the node power-off: unmanaged workloads, and applications that did not stop in time. We recommend that the cluster build team adds the graceful node shutdown of the kubelet as a second stop layer for these pods.

## What the kubelet does

When the operating system of a node starts a shutdown, the kubelet stops the pods on that node. With `shutdownGracePeriodByPodPriority`, it stops the pods in groups by PriorityClass. The group with the lowest priority stops first. Each group gets its own time.

The kubelet acts only on a shutdown signal of the operating system. It holds a systemd inhibitor lock to delay the shutdown. It does nothing when the power is pulled. It orders pods only on its own node. It does not replace the operator.

The feature `GracefulNodeShutdownBasedOnPodPriority` is beta and on by default.

## Recommended settings

1. Give each application a PriorityClass. Give a dependency, for example a database, a higher priority than the applications that use it. The kubelet then stops the dependents first, as the operator does.
2. Add `shutdownGracePeriodByPodPriority` to the kubelet configuration of each node. For example:

        shutdownGracePeriodByPodPriority:
          # system-cluster-critical, system-node-critical, and the operator.
          - priority: 2000000000
            shutdownGracePeriodSeconds: 20
          # Databases and other dependencies.
          - priority: 100000
            shutdownGracePeriodSeconds: 120
          # Applications that use the dependencies.
          - priority: 10000
            shutdownGracePeriodSeconds: 60
          # All other pods.
          - priority: 0
            shutdownGracePeriodSeconds: 30

3. Do not set `shutdownGracePeriod` or `shutdownGracePeriodCriticalPods` together with `shutdownGracePeriodByPodPriority`.
4. Make sure that the sum of all `shutdownGracePeriodSeconds` values fits in the time before the node power-off.
5. Make sure that the systemd-logind setting `InhibitDelayMaxSec` is not shorter than that sum. Check that no other package, for example `unattended-upgrades`, sets a lower value.

The operator chart uses the PriorityClass `system-cluster-critical` by default, so the kubelet stops the operator last.

Refer to the Kubernetes documentation: [Node shutdowns](https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/).
