# Tucunar Cell

Open-core operational monitoring from Tucunar Labs. Run the Go services and
ClickHouse on your own Linux machine, ingest signals through a versioned API,
evaluate threshold rules and inspect readings locally. Community edition is
Apache-2.0 licensed and runs without a Tucunar account or license server.

Version 0.1.0-alpha.1 is an early-access release for development and supervised
pilots. The native Linux console includes water, compressed air, electrical network
and grinding/flotation reference views. Water connectivity is observed;
electrical relationships follow the simulator model. Validate your own sources and rules before deploying.

## Community and commercial offering

| Community, Apache-2.0 | Commercial, separately contracted |
| --- | --- |
| Local ingestion, ClickHouse ledger, threshold evaluator and incident lifecycle | Installation, source mapping, upgrades and operational support |
| Native console source and the four SCADA reference readers | Managed deployments and response commitments agreed in the SOW |
| Versioned contracts, local tokens, diagnostics and k0s manifests | Hosted organization portal and billing remain in Tucunar's private control plane |

Every capability distributed here stays usable locally. The public core has no
user-count limit, expiry or paid unlock. Commercial service scope and support
hours are agreed before an engagement. Future enterprise capabilities are
announced when available.

## First run: source alpha on Linux

This release is for developers and supervised pilots. Installation currently
builds images locally; a prebuilt, signed public image bundle is not assumed.
Verify a downloaded source archive with its adjacent `.sha256` file using
`sha256sum -c <archive-name>.sha256` before extracting it, then run
`sha256sum -c SHA256SUMS` inside the extracted directory. A checksum supplied
alongside a download detects corruption; it does not authenticate its publisher.

Commands below run from the **root of the extracted source archive**, the
folder containing `contracts/` and `tools/`. No Tucunar account is required.

You will ingest a synthetic temperature reading, observe an incident when it
crosses 80 C, and send healthy readings until the incident becomes inactive.
The first download/build depends on your network; the incident exercise takes
about 90 seconds after installation. These are not measured installation-time
or production-capacity guarantees.

### 1. Prepare a dedicated host

The rehearsed platform is **Linux amd64** (Debian 13). Use systemd, sudo access, at least 2 CPUs, 4 GiB RAM and 25 GiB free
disk. Use one cell per cluster: this profile creates cluster-scoped local
volumes under `/var/lib/tucunar/` and the namespace `tucunar-cell`. Do not apply
it over an existing installation with different credentials.

Install Python 3.10+, GNU Make, k0s, kubectl, and nerdctl with BuildKit.
Use the upstream [k0s installation guide](https://docs.k0sproject.io/latest/install/),
[nerdctl downloads and dependencies](https://github.com/containerd/nerdctl#install)
and [BuildKit setup guide](https://github.com/containerd/nerdctl/blob/main/docs/build.md)
for prerequisites; the commands below configure them for Cell. k0s owns
containerd; **do not install another containerd daemon**. Configure BuildKit
against `/run/k0s/containerd.sock` in namespace `k8s.io`; see the
[k0s operations guide](tools/tucunar-cell/deploy/k0s/README.md).
Go is needed only for source checks and custom Go development, at the version
in `tools/tucunar-cell/go.mod`; image builds obtain their Go toolchain inside
the build image. The optional desktop additionally needs Rust, Raven and Vulkan.

For Linux amd64, the included downloader obtains the pinned nerdctl, BuildKit
and kubectl binaries from upstream and verifies their SHA-256 values against
`tools/tucunar-cell/community-release.json`. It extracts only the required
executables and never installs containerd. On the new dedicated host:

```bash
python3 tools/tucunar-cell/scripts/community-tools.py "$HOME/cell-tools"
for tool in nerdctl buildkitd buildctl kubectl; do
  sudo install -m 0755 "$HOME/cell-tools/$tool" /usr/local/bin/
done
```

The downloader refuses an existing destination directory. Preserve the verified
binaries for offline preparation or choose a new directory for a later version.


For a new host, the included bootstrapper can install a pinned k0s version and
prepare its kubeconfig. Review its actions first; do not use this to reconfigure
an existing cluster:

```bash
bash tools/tucunar-cell/deploy/install.sh --k0s-version v1.35.8+k0s.1 --skip-apply --dry-run
sudo bash tools/tucunar-cell/deploy/install.sh --k0s-version v1.35.8+k0s.1 --skip-apply
kubectl get nodes
sudo bash tools/tucunar-cell/scripts/containerd.sh check
```

Start BuildKit as described in the linked guide before building images:

```bash
sudo buildkitd --config tools/tucunar-cell/deploy/k0s/buildkit-k0s.toml.example
```

Keep that terminal open. Reuse an existing correctly configured BuildKit service
if one already owns `/run/buildkit/buildkitd.sock`.

The bootstrapper's copied `single-node` profile is not the Community instance.
Leave it unapplied; generate Community below. `containerd.sh check` verifies
containerd access; the image build is the actual BuildKit check.

### 2. Build and initialize

```bash
for service in swift condor canary-evaluator hummingbird; do
  sudo make -C tools/tucunar-cell "image-$service" VERSION=0.1.0-alpha.1 \
    CONTAINERD_ADDRESS=/run/k0s/containerd.sock
done
python3 tools/tucunar-cell/scripts/community-init.py "$HOME/.local/share/tucunar-cell"
kubectl kustomize "$HOME/.local/share/tucunar-cell/k0s/overlays/community" >/dev/null
kubectl apply -k "$HOME/.local/share/tucunar-cell/k0s/overlays/community"
kubectl -n tucunar-cell rollout status statefulset/clickhouse --timeout=180s
for service in swift condor canary-evaluator hummingbird; do
  kubectl -n tucunar-cell rollout status "deployment/$service" --timeout=180s
done
kubectl -n tucunar-cell port-forward --address=127.0.0.1 service/swift 18080:8080
```

Leave the last command running. Keep the generated directory private and back
it up: it contains stable instance identity, database credentials and operator
and ingest tokens. Initialization refuses to overwrite it. Reuse it on restarts.
Do not post rendered manifests or `secret.env` in issues.

Only the four core processes and ClickHouse run. Optional birds have zero
replicas. AI and public dashboard access are disabled. Cloud reporting remains
idle until explicitly configured. The host needs internet for initial build
and upstream images; local operation does not require Tucunar Cloud.

### 3. First signal, incident and recovery

In another terminal, from the same source root:

```bash
python3 tools/tucunar-cell/scripts/community-smoke.py \
  --secrets "$HOME/.local/share/tucunar-cell/k0s/overlays/community/secret.env" \
  --incident
```

Expected output reports readiness, rejected unauthenticated requests,
persistent signal readback, an active incident, and then an inactive incident
following healthy readings. Each run uses a unique synthetic tenant/scenario;
it retains its evidence and does not delete existing data.

The bundled `community.temperature.high` rule requires at least three samples
and uses a **60-second maximum**. The exercise sends three distinct hot readings.
A healthy reading cannot immediately cancel a hot reading still in that
window. The script continues healthy readings while that window expires and
fails with a useful message if the evaluator does not respond. Inactive means
the incident is absent from the active-incident API; this smoke test does not
assert durable recovery history or notification delivery. Omit `--incident`
for a short intake/readback-only check.

### 4. Connect your own source

Start from the [canonical signal example](tools/tucunar-cell/README.md#señales-genéricas)
and send `POST /v1/signals` with `Authorization: Bearer` using the
`SIGNAL_INGEST_TOKEN` in your private `secret.env`. Send UTC `occurred_at`, a
stable source/entity, and a unique `external_id` for each reading. Never put
tokens in URLs or source control. The node supplies its configured cell scope.

Here is a small standard-library sender you can adapt. It reads the token from
your private instance file rather than placing it in shell history:

```bash
python3 - <<'PYTHON'
from datetime import datetime, timezone
import json
from pathlib import Path
from urllib.request import Request, urlopen
import uuid

secrets = Path.home() / ".local/share/tucunar-cell/k0s/overlays/community/secret.env"
values = dict(line.split("=", 1) for line in secrets.read_text().splitlines()
              if line and not line.startswith("#"))
signal = {"source": "my-system", "tenant_id": "demo", "entity_id": "sensor-1",
          "kind": "measurement", "name": "temperature", "value": 25, "unit": "C",
          "external_id": uuid.uuid4().hex,
          "occurred_at": datetime.now(timezone.utc).isoformat()}
request = Request("http://127.0.0.1:18080/v1/signals",
                  data=json.dumps(signal).encode(),
                  headers={"Content-Type": "application/json",
                           "Authorization": "Bearer " + values["SIGNAL_INGEST_TOKEN"]})
with urlopen(request, timeout=10) as response:
    print(response.status, response.read().decode())
PYTHON
```

Expect HTTP `202` and an accepted signal ID. The demo rule intentionally does
not match `my-system`; change the source in your rule and send at least three
readings to exercise your own threshold.

Replace the demo rule in your generated
`k0s/overlays/community/rules.json`, reapply that profile, then restart only the
evaluator to load the changed rule:

```bash
kubectl apply -k "$HOME/.local/share/tucunar-cell/k0s/overlays/community"
kubectl -n tucunar-cell rollout restart deployment/canary-evaluator
kubectl -n tucunar-cell rollout status deployment/canary-evaluator --timeout=180s
```

Use the [rule format](tools/tucunar-cell/README.md#reglas-genéricas-de-umbral)
to match your source, measurement and units. The bundled incident exercise
requires the original Community rule.

### Troubleshooting and stopping

- **Pending pods:** `kubectl -n tucunar-cell get pods,pvc` and
  `kubectl -n tucunar-cell get events --sort-by=.lastTimestamp`. Check local
  volume availability and node resources.
- **ImagePullBackOff:** confirm the exact release tag was built into k0s'
  `k8s.io` namespace on the worker running the pod. A different containerd
  socket or namespace does not populate k0s' image store.
- **Connection refused:** keep the loopback port-forward running; check Swift's
  rollout and `kubectl -n tucunar-cell logs deployment/swift --tail=50`.
- **`No agent available` from port-forward or logs:** check `kubectl get nodes`
  and `sudo k0s status`. The Kubernetes API can answer while its connection to
  the worker is broken; pod listings alone do not prove live API access.
- **401:** use the secrets from the applied instance; keep operator and ingest
  tokens distinct. Do not enable public access to work around authentication.
- **No incident:** inspect
  `kubectl -n tucunar-cell logs deployment/canary-evaluator --tail=50`, check the
  bundled rule and current UTC clock. A source mismatch does not trigger it.

Stop the port-forward with Ctrl-C. To stop compute while preserving volumes:

```bash
kubectl -n tucunar-cell scale deployment --all --replicas=0
kubectl -n tucunar-cell scale statefulset/clickhouse --replicas=0
```

Reapply your generated Community profile to start again. Do not delete PVCs,
volumes or `/var/lib/tucunar/` to troubleshoot. Before upgrades, take a consistent
ClickHouse backup, retain the previous images and instance directory, and test
restore separately. Upgrade/rollback and offline recovery require an operational
rehearsal before relying on this alpha for production.

## Tested scope

The source-alpha onboarding was exercised on a clean Debian 13 amd64 KVM guest
with 2 vCPUs, 4 GiB RAM and a 40 GiB disk: cold image builds, persistent volumes,
all four service rollouts, Kubernetes port-forward, authenticated intake and
incident recovery. Restarting the four services preserved the synthetic ledger.
This is an installation baseline, not a sustained ingestion-capacity benchmark.

Offline OCI bundle export/signing (`community-bundle.py`), air-gapped verification
and import into k0s containerd (QEMU `restrict=on` with outbound network blocked,
verifying Ed25519 signatures and rejecting tampered manifests or untrusted signers),
consistent quiesced ClickHouse backup (872 signals, 2 incidents, 0.22s archive,
27.31s write interruption), schema-compatible upgrade (`0.1.0-alpha.1` → `0.1.0-alpha.2`)
and rollback, and preflighted separate-host restore (36.33s, exact row-count match)
were exercised on Debian 13 amd64 KVM guests (4 vCPUs, 3 GiB RAM) using an ephemeral
rehearsal signing key. Public release promotion still requires configuring the
production signing key secret in `tucunar-labs/cell`.

The exported native console was built with pinned Raven v0.2.2 and reviewed in
an isolated nested Wayland desktop using Mesa Lavapipe and fresh synthetic
readings from that cell. This does not validate customer GPUs, simulator
connectors, `linux/arm64` runtime parity, or sustained production load.

## Native console and SCADA reference

Build the native console with Rust and Raven v0.2.2, then point it at the local
forward and private token file. See [console setup](tools/tucunar-cell-console/README.md)
and [four-protocol reference](tools/tucunar-cell/examples/scada/README.md).
The simulator remains a separate dependency with its own license. The core
accepts real customer signals through the same HTTP contract.

## Offline images and signature verification

Build or obtain the Linux amd64 OCI bundle from a connected build host that
has the four versioned service images and ClickHouse 24.3 in k0s containerd:

```bash
sudo python3 tools/tucunar-cell/scripts/community-bundle.py export \
  "$HOME/tucunar-cell-oci-0.1.0-alpha.1" \
  --version 0.1.0-alpha.1 \
  --signing-key /path/to/release_ed25519
```

On the disconnected target host, verify the manifest, OCI archive contents,
SHA-256 checksums and OpenSSH Ed25519 signature against your trusted
`allowed_signers` file before importing into k0s containerd:

```bash
python3 tools/tucunar-cell/scripts/community-bundle.py verify \
  "$HOME/tucunar-cell-oci-0.1.0-alpha.1" \
  --allowed-signers /path/to/trusted/allowed_signers \
  --require-signature
sudo python3 tools/tucunar-cell/scripts/community-bundle.py import \
  "$HOME/tucunar-cell-oci-0.1.0-alpha.1" \
  --allowed-signers /path/to/trusted/allowed_signers \
  --require-signature
```

`verify` and `import` run without network access and reject tampered archives,
missing service images, untrusted signing keys or missing signatures. Never
trust an `allowed_signers` file fetched from the same unauthenticated channel
as the archive without verifying its fingerprint out-of-band.

After import, initialize or reapply your private Community instance directory.
Currently only `linux/amd64` is operationally rehearsed; `linux/arm64` is not
claimed as validated until exercised on matching hardware.

## Backup, separate-host restore, upgrade and rollback

Take a consistent quiesced ClickHouse backup before upgrading. Credentials stay
in your private instance directory and are never copied into the ClickHouse
archive:

```bash
sudo KUBECONFIG="$HOME/.kube/config" python3 tools/tucunar-cell/scripts/community-ops.py backup \
  --instance "$HOME/.local/share/tucunar-cell" \
  --output "$HOME/tucunar-cell-backups"
```

This scales the four services and ClickHouse to zero, records table row counts,
archives `/var/lib/tucunar/clickhouse/data` with SHA-256 checksums and a
manifest, then restores service availability and reports the observed write
interruption.

To restore onto a separate clean host, copy the backup directory and confirm
the target context, namespace and `cell_id` explicitly (preflight refuses
mismatched clusters, mismatched identities, corrupted archives or non-empty
target databases unless explicitly overridden):

```bash
sudo KUBECONFIG="$HOME/.kube/config" python3 tools/tucunar-cell/scripts/community-ops.py restore \
  --backup-dir "$HOME/tucunar-cell-backups/tucunar-cell-backup-<timestamp>" \
  --instance "$HOME/.local/share/tucunar-cell" \
  --expect-context default \
  --expect-namespace tucunar-cell \
  --confirm-cell-id "<target-cell-id>" \
  --allow-cross-cell
```

To upgrade a running cell after importing or building the target version's
images locally, preserve the previous overlay and verify `/version` and
`/readyz` across all four services:

```bash
sudo KUBECONFIG="$HOME/.kube/config" python3 tools/tucunar-cell/scripts/community-ops.py upgrade \
  --instance "$HOME/.local/share/tucunar-cell" \
  --target-version 0.1.0-alpha.2 \
  --require-local-images
```

To roll back a **schema-compatible** binary upgrade (`schema_compatibility`
unchanged), restore the snapshot saved under `<instance>/history/`:

```bash
sudo KUBECONFIG="$HOME/.kube/config" python3 tools/tucunar-cell/scripts/community-ops.py rollback \
  --instance "$HOME/.local/share/tucunar-cell" \
  --to-snapshot "$HOME/.local/share/tucunar-cell/history/<timestamp>-0.1.0-alpha.1" \
  --require-local-images
```

If a release changes `schema_compatibility`, binary-only rollback is refused:
restore the pre-upgrade ClickHouse backup first, then run `rollback` with
`--after-restore`.

## Verify the source distribution

From the source checkout, run:

```bash
python3 tools/tucunar-cell/scripts/verify-export.py
```

This exports outside the checkout, verifies file checksums and entry-document
links, re-exports without private files, rejects a synthetic `.env` sentinel,
and runs Go tests, builds and vet from the exported modules. The Community
source CI runs the same command. Its native-console and reference-reader jobs
must also pass before it packages a source candidate. Those jobs test source
and bindings; they do not certify a GPU or a live SCADA endpoint. This does not validate k0s installation,
ClickHouse integration, the native desktop or offline recovery; those require
an operational release rehearsal. Archive checksums detect corruption; verify
publisher identity separately before trusting a download.

## Operation and development

- [Runtime, configuration and API](tools/tucunar-cell/README.md)
- [k0s installation and offline images](tools/tucunar-cell/deploy/k0s/README.md)
- [Signal and service contracts](contracts/tucunar-capabilities.openapi.yaml)
- [Authenticated native console API](contracts/tucunar-cell-console.openapi.yaml)
- [Contributing](CONTRIBUTING.md) and [security reporting](SECURITY.md)
- [Third-party notices](NOTICE)

```bash
(cd contracts && go test ./...)
(cd tools/tucunar-cell && go test ./... && go build ./... && go vet ./...)
(cd tools/tucunar-cell-console && cargo test --locked && cargo build --release --locked)
```

Keep raw data and evaluation within the customer's perimeter. Cloud reporting
is optional and outbound-only. Back up ClickHouse with a consistent snapshot
or a configured ClickHouse BACKUP destination before upgrades. Test restore
on a separate machine. Single-node deployments require recovery planning.

For supported installation and operation, contact the Tucunar team through
[Tucunar Labs](https://tucunar.com/cell).
