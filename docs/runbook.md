# Runbook for pod-shutdown-operator

This runbook is for the platform engineers of a cluster that runs the operator. The commands use the namespace `giantswarm`. Use the namespace of your installation.

## Call the HTTP endpoint

The endpoint uses a `NodePort` service. The default port is `30082` (the value `api.nodePort`). Any node address works.

Get the token. Give it to the callers at setup. The callers do not read the Secret.

    kubectl --namespace giantswarm get secret pod-shutdown-operator-api-token --output jsonpath='{.data.token}' | base64 -d

When you set `api.existingSecret`, use the name of that Secret.

Start a shutdown:

    curl --request POST --header "Authorization: Bearer $TOKEN" http://<node-address>:30082/shutdown

Cancel a shutdown. The operator then starts the scale-up from the record:

    curl --request POST --header "Authorization: Bearer $TOKEN" http://<node-address>:30082/cancel

Read the progress. This call needs no token:

    curl http://<node-address>:30082/progress

Refer to [HTTP API](http-api.md) for the responses, the phase rules, and the progress fields.

## Read the state of the last shutdown

The operator keeps the state of the last shutdown in the `ShutdownRequest` with the name `cluster`:

    kubectl get shutdownrequest cluster --output yaml

The field `status.phase` is `ShuttingDown`, `Complete`, `ScalingUp`, or `Restored`. The field `status.recordedState` holds the replica counts from the start of the shutdown.

## Start a shutdown again after a false power cycle

At each start, the operator compares the boot ID of its node with the record in `status.bootIDs`. A different boot ID means a power cycle. The operator then sets the action `Cancel` and scales the cluster up from the record.

The operator also does this when the record has no entry for its node. This occurs when the leader moves to a node that joined after the start of the shutdown, or to a node that had an empty boot ID at that time. The operator cannot tell this from a power cycle. It scales up on purpose, because a wrong guess the other way keeps the cluster at 0 after a power-on.

The log of the new leader then shows the message `Found a power cycle, scales up from the record` with an empty `recordedBootID`. The empty value shows that the record had no entry for the node:

    kubectl --namespace giantswarm logs --selector app.kubernetes.io/name=pod-shutdown-operator --tail -1 | grep 'Found a power cycle'

Do these steps:

1. Make sure that no power cycle occurred. Most nodes must have an uptime that is longer than the time since the start of the shutdown.
2. If the shutdown must go on, call `POST /shutdown` again. In `ScalingUp`, the operator returns to the shutdown with the same record. In `Restored`, the call starts a new shutdown.

The operator writes the boot ID of the new leader node to the record. A later restart on the same boot then does not cancel the shutdown again.

## Scale the Flux controllers back by hand

The operator scales the Flux controllers to 0 at the start of a shutdown. It scales them back at the end of the scale-up. If the operator cannot do this, for example because it crashes in a loop, the cluster stays with Flux at 0. Then Flux cannot repair anything.

Do these steps to scale the Flux controllers back by hand:

1. Check the operator pods. Use this procedure only when the operator cannot run:

        kubectl --namespace giantswarm get pods --selector app.kubernetes.io/name=pod-shutdown-operator

2. Scale each Flux controller to the replica count in the record:

        kubectl get shutdownrequest cluster \
          --output jsonpath='{range .status.recordedState.fluxControllers[*]}{.namespace} {.name} {.replicas}{"\n"}{end}' |
          while read -r namespace name replicas; do
            kubectl --namespace "$namespace" scale deployment "$name" --replicas "$replicas"
          done

3. If the record is not available, scale the default Flux controllers to 1:

        kubectl --namespace flux-system scale deployment kustomize-controller helm-controller --replicas 1

When `kustomize-controller` runs again, the Flux bootstrap Kustomization can apply the Flux manifests again with `replicas: 1`.
