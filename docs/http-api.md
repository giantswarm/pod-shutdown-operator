# HTTP API

Every operator pod serves the endpoint.
The chart exposes it on the node port `api.nodePort` (default `30082`) of each node.
The endpoint uses plain HTTP, so the token goes over the network in clear text.
Keep the node addresses in a trusted network.
The endpoint has no rate limit, because a limit lets any client block `POST /shutdown` during a power failure.
Thus use a token of at least 32 random bytes, so that nobody can guess it.

The binary flags are `--api-bind-address` (default `:8082`, empty disables the endpoint) and `--api-token-file`.
The operator reads the token file at each call, so a new token needs no restart.
With no readable token, the endpoint refuses each call that needs a token.

## Calls

| Call | Token | Description |
|---|---|---|
| `POST /shutdown` | Yes | Start a shutdown. |
| `POST /cancel` | Yes | Cancel a shutdown, and start the scale-up. |
| `GET /progress` | No | Read the progress. |

Send the token in the header `Authorization: Bearer <token>`.

## Responses

| Code | Meaning |
|---|---|
| `202` | The operator stored the intent. The leader acts on it later. A repeat call writes nothing. |
| `401` | The token is missing or not correct. |
| `500` | The write failed. Send the call again. |

## Phase rules

What a call does in each phase of the `ShutdownRequest`:

| Phase | `POST /shutdown` | `POST /cancel` |
|---|---|---|
| No request | Create a request. | Nothing. |
| No phase | Nothing. The leader starts the shutdown. A request older than `staleLimit` is replaced with a new one. | Nothing. |
| `ShuttingDown`, `Complete` | The shutdown continues. | Start the scale-up. |
| `ScalingUp` | Return to `ShuttingDown` with the same record. | The scale-up continues. |
| `Restored` | Replace the request, and start a new shutdown. | Nothing. |

When two calls come before the leader acts, the last call wins.

## Progress

`GET /progress` returns JSON:

```json
{"phase": "ShuttingDown", "complete": false, "budgetRemainingSeconds": 412, "stopped": 7, "total": 12}
```

| Field | Description |
|---|---|
| `phase` | The phase of the `ShutdownRequest`. Empty when no request exists. |
| `complete` | True when the cluster is ready for the power-off. Refer to [complete](#complete). |
| `budgetRemainingSeconds` | The time to the end of the budget. Only in `ShuttingDown` and `Complete`. It ignores the safety margin. |
| `stopped` | The number of applications that have stopped. |
| `total` | The number of applications in the record. |

### complete

`complete` is true only in the phase `Complete`, and only after the current leader has checked that no power cycle occurred since the shutdown.
After an operator start or a leader change, `complete` is false until the new leader has done this check.
The leader writes its identity to `status.bootCheckedBy` when the check finds no power cycle.
`complete` is true when this identity is equal to the holder of the leader election Lease.
With leader election off, only the pod that did the check shows `complete` as true.

Use `complete`, not `phase`, to decide when to power off the nodes.

## Call again after a power-on

When the cluster powers on with a request that is not `Restored`, the leader finds the power cycle at its first reconcile.
It then sets the action `Cancel`.
A `POST /shutdown` that comes before this check is lost, but it still returns `202`.
A mains supply that fails again soon after a power-on can cause this.

After a `POST /shutdown`, read `GET /progress` until `complete` is true.
If the phase changes to `ScalingUp` or `Restored`, send `POST /shutdown` again.
