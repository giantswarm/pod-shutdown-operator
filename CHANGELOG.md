# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Changed

- feat: operator scaffold and basic shutdown
- feat: scale up from the record on cancel
- feat: stop and start applications in dependency order
- feat: stop each application inside the time budget
- feat: detect a power cycle at operator start
- feat: stop the Flux controllers first and restore them last
- feat: add the ShutdownPlan dry run
- feat: add the cluster checks to the ShutdownPlan dry run
- feat: package the operator, and add the kind e2e test and the runbook
- fix: confirm a cached 0 before a stop, and do not block the scale-up on one failed start
- fix: return the Node list error at record time, and keep the stale check until a request passes it
- docs: keep a missing own boot ID entry as a power cycle, and tell what to do in the runbook
- feat: generate the API token from 32 random bytes in the chart

### Fixed

- fix: try each Flux controller and requeue at the poll interval after a failed Flux start
- fix: replace an old request with no phase on POST /shutdown, so that the stale rule does not drop the call
- fix: do not halt a RerunOnFailure VM whose VMI is already in deletion
- fix: read the VM terminationGracePeriodSeconds from the running VMI
- fix: show complete in GET /progress only after the boot ID check of the current leader
- fix: remove the double period from the finding messages, and correct the stop and start comments


[Unreleased]: https://github.com/giantswarm/pod-shutdown-operator/tree/main
