# monitoring-pmm

OpenEverest monitoring extension for [Percona Monitoring and Management](https://docs.percona.com/percona-monitoring-and-management/) (PMM) 3.

It installs the `pmm` `MonitoringClass` and its controller. Instances whose provider integrates PMM natively (Percona XtraDB Cluster, Percona Server for MongoDB, Percona PostgreSQL) can then send their metrics and Query Analytics to a PMM 3 server by listing a `pmm` `MonitoringDestination` in `spec.monitoring.destinations[]`.

- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Install](#install)
- [Connect a PMM server](#connect-a-pmm-server)
- [Send an Instance's monitoring to PMM](#send-an-instances-monitoring-to-pmm)
- [Destination status](#destination-status)
- [Kubernetes infrastructure metrics](#kubernetes-infrastructure-metrics)
- [Chart values](#chart-values)
- [Compatibility](#compatibility)
- [Development](#development)

## How it works

`pmm` is a **ProviderManaged** class: the database provider renders the engine operator's native PMM integration itself, so nothing in this repository touches database pods. This controller only validates the PMM server and token behind each destination and, through the class, owns the pmm-client image the providers run.

- **This controller** claims the MonitoringClass named by `class.name` (`--class-name`) and sets `Accepted=True` on it; any other class is left unclaimed. It keeps the `Ready` condition and `status.serverVersion` of every destination of that class current by calling the PMM server's version API and its settings API, which only the Admin role reads, with the destination's token. It re-checks a `Ready` destination every 5 minutes. A failing one is re-checked after as long as it has been failing, at least every minute and at most every 5 minutes. Those periodic checks also pick up a rotated token; a change to the destination or the class re-checks it immediately.
- **The provider** renders pmm-client for every Instance entry that points at a `pmm` destination: the image is the class's `agentImage`, the server is the destination's `url`, the token is copied from the destination Secret into the secret the Percona operator reads it from, and the container resources are the Instance size tier merged with the entry's `parameters.resources`. The Percona operator then runs pmm-client as a sidecar of the database pods, which registers them with PMM (metrics and Query Analytics). Adding, removing or changing the agent restarts the database pods one at a time.
- **provider-runtime** reports whether that worked on the `MonitoringBinding` (`Configured`) and in the Instance's `MonitoringConfigured` condition.

The controller writes the class's `Accepted` condition and each destination's `Ready` condition and `status.serverVersion`, and nothing else: it creates no objects and never writes a binding, an Instance or a Secret.

PMM 3 only: a PMM 2 server is reported as `UnsupportedServerVersion`.

## Requirements

- OpenEverest with the monitoring API (`monitoringclasses`, `monitoringdestinations` and `monitoringbindings` in `monitoring.openeverest.io`), see [Compatibility](#compatibility).
- A provider that declares the `pmm` integration in `Provider.spec.monitoring`.
- A PMM 3 server reachable from the cluster (the controller and the database pods connect to it).

## Install

```sh
helm install monitoring-pmm oci://ghcr.io/openeverest/charts/monitoring-pmm -n openeverest-system \
  --set class.agentImage=percona/pmm-client:3.8.0 \
  --set class.agentVersion=3.8.0
kubectl get monitoringclass pmm   # ACCEPTED shows Accepted once the controller has claimed the class
```

`class.agentImage` and `class.agentVersion` are required values. They are the pmm-client image every provider runs next to the databases that send to a `pmm` destination, and its version, which each provider checks against the pmm-client versions it supports; providers ship no PMM image of their own. The chart defaults them to `percona/pmm-client:3.8.0` and `3.8.0`, so the install works without the two `--set` flags, but setting them pins the agent independently of chart upgrades and lets you point at a mirrored image. Setting either one empty fails the install with `class.agentImage is required` (or `class.agentVersion is required`). Changing them later restarts the database pods of every Instance that sends to a `pmm` destination.

OpenEverest pre-installs no monitoring backend: the `pmm` class exists once this chart is installed, and destinations of it stay `Ready=Unknown` (`Pending`) until this controller has checked them.

## Connect a PMM server

### 1. Create a PMM 3 service-account token

The providers register database nodes and services with PMM using this token, so the service account needs the **Admin** role. The controller checks it: a token without the Admin role, such as a Viewer token, leaves the destination `Ready=False` with reason `Unauthorized`.

In the PMM UI: **Administration → Users and access → Service accounts → Add service account**, choose the Admin role, then **Add service account token** and copy the token (it starts with `glsa_`).

Or with the API, as a PMM administrator:

```sh
PMM=https://pmm.example.com
curl -fsS -u admin:<password> -H 'Content-Type: application/json' \
  -X POST "$PMM/graph/api/serviceaccounts" -d '{"name": "openeverest", "role": "Admin"}'
# Use the "id" from the response:
curl -fsS -u admin:<password> -H 'Content-Type: application/json' \
  -X POST "$PMM/graph/api/serviceaccounts/<id>/tokens" -d '{"name": "openeverest"}'
# The "key" in the response is the token.
```

### 2. Store the token in a Secret

Create it in the namespace of the Instances to monitor, under the key `token`:

```sh
kubectl -n team-a create secret generic pmm-prod-token --from-literal=token=glsa_...
```

Several destinations, of any class, may share a Secret.

### 3. Create the destination

```yaml
apiVersion: monitoring.openeverest.io/v1alpha1
kind: MonitoringDestination
metadata:
  name: pmm-prod
  namespace: team-a
spec:
  classRef:
    name: pmm
  url: https://pmm.example.com
  authentication:
    mode: ServiceAccountToken
    credentialsSecretRef:
      name: pmm-prod-token
```

| Field | Required | Description |
|---|---|---|
| `url` | yes | PMM server URL, `http://` or `https://`, for example `https://pmm.example.com`. |
| `authentication.mode` | yes | `ServiceAccountToken`, the class's only mode. |
| `authentication.credentialsSecretRef` | yes | The Secret holding the token under the key `token`. |
| `tls.ca.secretKeyRef` | no | A Secret key holding the PEM CA bundle the server certificate is verified against, for a PMM server behind a private CA. |
| `tls.insecureSkipVerify` | no | Do not verify the server certificate. Only for a self-signed PMM server; it cannot be set together with `tls.ca`. |

`tls` applies to this controller's check of the server only. pmm-client, as the database operators run it, does not verify the PMM server certificate whatever `tls` says, and every binding to a `pmm` destination reports `Configured=True` with the reason `AgentTLSNotVerified` to say so.

```sh
kubectl -n team-a get monitoringdestination pmm-prod
# NAME       CLASS   ACCEPTED   READY   VERSION   AGE
# pmm-prod   pmm     Accepted   Ready   3.8.0     1m
```

## Send an Instance's monitoring to PMM

List the destination in the Instance:

```yaml
spec:
  monitoring:
    destinations:
      - name: pmm
        destinationRef:
          name: pmm-prod
        parameters:          # optional
          resources:
            limits:
              memory: 256Mi
```

`parameters.resources` sets the pmm-client container's requests and limits on top of the Instance size defaults. Removing the entry removes pmm-client from the database pods.

## Destination status

| Condition | Written by | Meaning |
|---|---|---|
| `Accepted` | OpenEverest core | The class exists, `url` is set, the mode is `ServiceAccountToken`, the Secret carries `token` and the CA bundle, if any, parses (`ClassNotFound`, `URLRequired`, `AuthModeUnsupported`, `CredentialsNotFound`, `InvalidCABundle`). |
| `Ready` | this controller | The PMM server was reachable and the token has the Admin role. |

`Ready` reasons:

| Reason | Status | Cause |
|---|---|---|
| `Ready` | True | PMM 3 answered with its version and served its settings, which only the Admin role reads. |
| `Unauthorized` | False | PMM rejected the token (HTTP 401 or 403), the token lacks the Admin role pmm-agent needs (the settings API answered 401 or 403), or there is no token to send: no `authentication.credentialsSecretRef`, the Secret does not exist or has no `token` key. |
| `UnsupportedServerVersion` | False | The server is not PMM 3 (`serverVersion` shows what it is). |
| `InvalidParameters` | False | `url` is missing or not an `http://` or `https://` URL with a host, the mode is not `ServiceAccountToken`, or the CA Secret or key named by `tls.ca` is missing or not a PEM bundle. |
| `Unreachable` | False | Connection, DNS or TLS error, an address the check refuses (see [Server check limits](#server-check-limits)), a redirect, an unexpected HTTP status, or a reply that is not a PMM version. |
| `Pending` | Unknown | Not checked yet: the controller is not running or has not reached this destination yet. |

`serverVersion` keeps the last version the server reported while it is unreachable or rejects the token.

### Server check limits

A destination can name any Secret in its namespace, and the check sends that Secret's `token` to the destination's `url`. Creating or updating a `pmm` destination is therefore a privileged action: grant it only to those trusted with the Secrets of that namespace. The check itself is limited so that a destination cannot turn it against the cluster:

- `url` must be `http://` or `https://` with a host.
- Redirects are not followed: a 3xx reply is `Unreachable` and its message names only the target host. No HTTP proxy is used.
- The controller resolves the host itself and does not connect if any of its addresses is link-local (`169.254.0.0/16`, `fe80::/10`, which include cloud metadata endpoints) or unspecified (`0.0.0.0`, `::`). Loopback addresses (`127.0.0.0/8`, `::1`, `localhost`) are refused too unless the chart value `probe.allowLoopback` is `true`, for a PMM server running next to the controller in development.
- At most 64 KiB of a reply is read. Reply bodies and headers, apart from a redirect's target host, never reach the condition, whose message is capped at 1 KiB.

## Kubernetes infrastructure metrics

This extension sends database metrics and Query Analytics to PMM. Kubernetes infrastructure metrics (nodes, pods, kube-state-metrics) are not part of it: shipping them to PMM is a separate, manual setup outside OpenEverest, and this chart neither installs nor manages it. That setup takes:

- [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics), for the state of Kubernetes objects;
- a metrics agent remote-writing to PMM's VictoriaMetrics at `<pmm-url>/victoriametrics/api/v1/write` with a PMM service-account token: either the [VictoriaMetrics Operator](https://docs.victoriametrics.com/operator/) with a `VMAgent`, or a standalone vmagent;
- what that agent scrapes: kube-state-metrics, and the kubelet and cAdvisor on every node (with the operator, a `VMServiceScrape` and `VMNodeScrape`s).

## Chart values

| Value | Default | Description |
|---|---|---|
| `class.agentImage` | `percona/pmm-client:3.8.0` | **Required.** pmm-client image the providers run next to each database. Changing it restarts the database pods of every Instance sending to a destination of this class. |
| `class.agentVersion` | `3.8.0` | **Required.** Version of `class.agentImage`, matched against the pmm-client versions each provider supports. |
| `class.create` | `true` | Install the `MonitoringClass`. |
| `class.name` | `pmm` | Name of the class destinations reference, and the only class the controller claims. |
| `class.displayName` | `Percona Monitoring and Management` | Name the UI shows for the class. |
| `image.repository` | `ghcr.io/openeverest/monitoring-pmm` | Controller image. |
| `image.tag` | chart `appVersion` | Controller image tag. |
| `leaderElection.enabled` | `true` | Leader election (needed with more than one replica). |
| `podSecurityContext`, `securityContext` | Pod Security Standard `restricted`, UID 65532 | Controller pod and container security context. |
| `probe.allowLoopback` | `false` | Let the server check connect to loopback addresses such as `localhost`. Link-local addresses, which include cloud metadata endpoints, are always refused. |
| `rbac.create` | `true` | Create the ClusterRole, the leader-election Role and their bindings. |
| `resources` | 20m/64Mi requests, 200m/128Mi limits | Controller resources. |

### Permissions

The controller reads `monitoringclasses` and `monitoringdestinations` and patches only their status. It also needs `get` on Secrets, and nothing more: it reads the token from a destination's credentials Secret each time it checks that destination, and never lists, watches or caches Secrets. It is the only monitoring extension that reads destination Secrets, and it never reads Instance credentials. Leader election uses a Role in the release namespace.

The Secret `get` is cluster-wide, a known limit of this release (OpenEverest 2.0.0-dev.5): destination tokens live in the users' namespaces, which are not known when the chart is installed, so the chart cannot ship a namespaced Role for them. Narrowing the grant to the Secrets destinations reference is later work.

## Compatibility

| monitoring-pmm | OpenEverest | PMM server | Default pmm-client |
|---|---|---|---|
| `0.1.x` | 2.0.0-dev.5 and later | 3.x | 3.8.0 |

## Development

```sh
make build              # bin/controller
make test-unit          # unit tests
make lint               # golangci-lint
make generate           # ClusterRole from the kubebuilder markers in internal/controller/rbac.go
make helm-lint helm-template
```

Integration tests (chainsaw) run the chart against a fake PMM server in k3d. The cluster holds only the OpenEverest CRDs, taken from the core module version pinned in `go.mod`, not core itself:

```sh
make k3d-cluster-up
make docker-build load-image IMG=monitoring-pmm:dev
make install-crds deploy-ci IMG=monitoring-pmm:dev
make test-integration
make k3d-cluster-down
```

Pushing a `vX.Y.Z` tag builds `ghcr.io/openeverest/monitoring-pmm:X.Y.Z` and publishes the chart with version and appVersion `X.Y.Z` to `oci://ghcr.io/openeverest/charts`; the [Compatibility](#compatibility) table must have a row for the tag's minor series. Every push to `main` builds `ghcr.io/openeverest/monitoring-pmm-dev`.

## License

Apache License 2.0, see [LICENSE](LICENSE).
