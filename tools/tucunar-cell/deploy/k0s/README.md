# k0s cell deployment

k0s is the cell supervisor. It restarts failed birds, exposes readiness and
liveness probes, provides service discovery and keeps the installation small
enough for a single Linux machine. It is not part of the data contract: a
customer can replace k0s later as long as the process contracts remain.

## Profiles

| Profile | Purpose | Secret handling |
|---|---|---|
| `overlays/dev` | local manifest inspection and disposable cluster | dev-only literals |
| `overlays/single-node` | first customer machine | `secret.env` outside Git |
| `base` | reusable cell resources | operator creates the Secret |

The base deploys one ClickHouse StatefulSet and independent Deployments for
Swift, the Canary evaluator, Condor, Hummingbird and every contract
stub. It intentionally starts with one replica for stateful/storage workloads;
stateless birds can scale independently once their implementation and
idempotency are proven.

The base also applies a default-deny NetworkPolicy. Pods may resolve DNS,
call the cell's HTTP/ClickHouse ports and receive the corresponding internal
traffic. Only Swift (external source adapters), Hummingbird (cloud reporting)
and Shiitake (optional AI provider) receive HTTPS egress; all other external
ports remain denied. NetworkPolicy enforcement must be verified with the
selected k0s CNI before using a multi-tenant node.

## Build images with k0s containerd

k0s includes and owns the containerd runtime. Do not install Docker Engine,
Docker Compose or a second containerd daemon for a cell. The `Containerfile`
in the repository is only an OCI image recipe; `nerdctl` sends it to BuildKit and
stores the result directly in k0s' `k8s.io` containerd namespace.

On a k0s server, install the standalone `nerdctl`, `buildkitd` and `buildctl`
binaries from the [nerdctl releases](https://github.com/containerd/nerdctl/releases)
and [BuildKit releases](https://github.com/moby/buildkit/releases), verifying their
upstream checksums. If using a bundled upstream archive, install those binaries without
installing or starting its containerd daemon. Configure the BuildKit worker
with the included `buildkit-k0s.toml.example` (containerd address and namespace
are explicit). If BuildKit is not already managed on this host, run it in a
separate terminal from the repository root:

```bash
sudo buildkitd --config tools/tucunar-cell/deploy/k0s/buildkit-k0s.toml.example
```

Leave it running during image builds. It listens on the local default BuildKit
socket `/run/buildkit/buildkitd.sock`; no TCP listener is needed. If an existing
BuildKit service owns that socket, review its configuration instead of starting
a competing service. The worker must use k0s' embedded containerd. Then check
the runtime and build either one bird or the whole cell:

```bash
cd tools/tucunar-cell
TUCUNAR_CONTAINERD_ADDRESS=/run/k0s/containerd.sock \
  sudo ./scripts/containerd.sh check
TUCUNAR_VERSION=0.1.0 \
  TUCUNAR_CONTAINERD_ADDRESS=/run/k0s/containerd.sock \
  sudo ./scripts/containerd.sh build condor
TUCUNAR_VERSION=0.1.0 \
  TUCUNAR_CONTAINERD_ADDRESS=/run/k0s/containerd.sock \
  sudo ./scripts/containerd.sh build-all
```

The k0s manifests use the same image names and `IfNotPresent`, so images built
in the local `k8s.io` namespace do not need a registry on a single-node cell:

```bash
kubectl apply -k deploy/k0s/overlays/dev
kubectl -n tucunar-cell rollout status deployment/swift
```

For multiple nodes, push the exact tags to an OCI registry and configure
`/etc/k0s/registries.yaml` on every node. The repository includes a
credential-free example at `deploy/k0s/registries.yaml.example`. Never put
provider credentials in image layers.

For a disconnected node, export images on a connected build host and import
the archive with the k0s containerd namespace:

```bash
sudo nerdctl --address /run/k0s/containerd.sock --namespace k8s.io \
  save -o tucunar-cell-images.tar \
  ghcr.io/tucunar/condor:0.1.0 ghcr.io/tucunar/canary-evaluator:0.1.0
sudo k0s ctr --namespace k8s.io images import tucunar-cell-images.tar
```

The image archive is OCI/containerd data; it does not require Docker. For a
full air-gapped k0s install, preload k0s' own images according to the k0s
air-gap procedure and preload the Tucunar images with the same mechanism.

## Install on a single Linux machine

The repository keeps the cluster configuration in `k0s.yaml`. It uses k0s's
managed containerd, the default kube-router network, and the managed etcd
control-plane store. A production multi-node installation should set
`spec.api.address` to the node address reachable by the other workers.

For a single Linux machine that remains expandable, use the repository
installer. Pin the k0s version explicitly; the installer never guesses a
version when it has to download k0s:

```bash
cd tools/tucunar-cell
sudo ./deploy/install.sh --k0s-version <pinned-k0s-version> --skip-apply
```

Fill the generated
`/opt/tucunar-cell/deploy/k0s/overlays/single-node/secret.env` with real values,
preload/build the pinned images, then apply and wait for readiness:

```bash
sudo ./deploy/install.sh --skip-k0s-install
```

For a manual installation or an operator-managed k0s host:

```bash
curl --proto '=https' --tlsv1.2 -sSf https://get.k0s.sh | sudo sh
sudo install -d -m 0755 /etc/k0s
sudo install -m 0644 k0s/k0s.yaml /etc/k0s/k0s.yaml
sudo k0s install controller --config /etc/k0s/k0s.yaml \
  --enable-worker --no-taints --start
sudo k0s kubeconfig admin | sudo install -m 600 /dev/stdin "$HOME/.kube/config"
```

The base includes explicit local static volumes for this one-host profile. The
paths are `/var/lib/tucunar/clickhouse/data` and
`/var/lib/tucunar/clickhouse/logs`; use a CSI provider and node-aware volumes
before expanding a cell across hosts.

Then prepare the Secret:

```bash
cd tools/tucunar-cell/deploy
cp k0s/overlays/single-node/secret.env.example \
  k0s/overlays/single-node/secret.env
$EDITOR k0s/overlays/single-node/secret.env
kubectl apply -k k0s/overlays/single-node
kubectl -n tucunar-cell get pods -w
```

The manifest expects the images named in `base/services-core.yaml` and
`base/services-capabilities.yaml` to be available to the cluster. For a local
k0s registry or an air-gapped customer, preload the pinned images and set the
image references in an overlay.

## Health and replacement

Every process exposes `/healthz` and `/readyz`. k0s restarts a process whose
liveness fails; it keeps a process out of service when readiness fails. A
customer replacement must expose the same health surface and implement the
versioned Connect contract in `contracts/proto/tucunar/v1`. Condor's Connect
Read path keeps bounded consumer cursors in ClickHouse; offsets expire after
90 days and are scoped to the organization and cell configured for the node.

`hummingbird` is the only component intended to call Tucunar Cloud. The cloud
does not open a route into the cell. Internal calls use Kubernetes Services and
the `TUCUNAR_INTERNAL_TOKEN` Secret until mTLS/service identity is added.

### Optional DeepSeek operator stories

The cell does not need an external model to ingest, evaluate, alert, or
recover. To enable the console's `Explain case` action, create the separate
AI secret out-of-band from a private env file (never commit it):

```bash
kubectl -n tucunar-cell create secret generic tucunar-cell-ai-secrets \
  --from-env-file=tools/tucunar-cell/.env \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n tucunar-cell rollout restart deployment/shiitake
```

The env file should contain `AI_REASONING_ENABLED=1`,
`DEEPSEEK_MODEL=deepseek-v4-flash`, and `DEEPSEEK_API_KEY`. The AI deployment
imports `tucunar-ai-secrets` (and the legacy `tucunar-cell-ai-secrets`) as
optional references; omitting them leaves the deterministic story fallback
active. Provider and channel credentials follow the same pattern through the
optional `tucunar-source-secrets` and `tucunar-channel-secrets` references.
The AI process also needs ClickHouse access because it reserves
provider calls in `canary_ai_call_ledger` before calling DeepSeek. The hard
budget is 2 calls per `scenario_id` and 10 calls per organization/cell per
rolling 24 hours. Keep `shiitake` at one replica; k0s can restart it, but a
second replica would require a distributed quota reservation mechanism.

## Data safety

- ClickHouse PVCs are durable and are not removed by an upgrade.
- Do not use `kubectl delete pvc` as an operational shortcut.
- Backups and restore tests remain mandatory before changing the storage
  profile.
- A cell can continue to ingest/evaluate locally when the control plane is
  unavailable; cloud reporting resumes through Hummingbird.
