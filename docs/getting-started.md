# Getting started

Do these steps in this order.
Each step links to the document with the details.

1. Read [How it works](how-it-works.md).
   It tells the phases of a shutdown, the time budget, and the scale-up.
2. Install the chart.
   Refer to [Install](install.md).
   Set `api.token`, or set `api.existingSecret`.
3. Write a `ManagedApplication` for each application.
   Refer to [Resources](resources.md#managedapplication).
   Set `dependsOn`, so that an application stops before its dependencies.
4. Create a `ShutdownPlan`, and read its findings.
   Refer to [Resources](resources.md#shutdownplan).
   Correct each finding with the severity `Error` before a real shutdown.
5. Give the token to the callers.
   Refer to [Runbook](runbook.md#call-the-http-endpoint).
6. Make the callers send `POST /shutdown`, and read `GET /progress` until `complete` is true.
   Refer to [HTTP API](http-api.md).
7. Read the [Runbook](runbook.md) for the recovery steps.
8. Give the [Kubelet recommendation](kubelet.md) to the cluster build team.
