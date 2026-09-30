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
kubectl apply -f deploy/class.yaml
go run ./cmd/controller            # uses the current kubeconfig
```
