# monitoring-pmm

PoC of a **ProviderManaged** `MonitoringClass` controller for spec 014
(extensible monitoring). It claims the `pmm` class and keeps every pmm
`MonitoringDestination`'s `Ready` condition and `serverVersion` current by
probing the PMM 3 server with the service-account token in the destination's Secret
(key `apiKey`).

Rendering `pmm-client` into the engine pods is the provider's job: providers
declaring the `pmm` integration in `Provider.spec.monitoring` render the
operator's native `spec.pmm` from the binding inside `Sync`, and
provider-runtime owns the binding's `Configured` condition.

```sh
# Installs the controller and claims the `pmm` MonitoringClass. Like a provider,
# it is its own chart, so it can ship as a hub extension or as a conditional
# subchart of the OpenEverest chart (the natural home for the PMM pipeline core
# ships today). `class.agentImage` is the pmm-client the providers render.
helm install monitoring-pmm charts/monitoring-pmm -n openeverest-system
```

It runs in-cluster because it has to reach the PMM Server's Service DNS name.
