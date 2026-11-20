# Cell runtime architecture

Cell runs independent Go processes with versioned contracts and ClickHouse as
the operational data plane. Community enables four services:

- **Swift** accepts authenticated source signals and normalizes them into the
  canonical signal contract. It sends durable appends to Condor.
- **Condor** owns the raw signal ledger and bounded reads through Connect.
- **Canary evaluator** rebuilds lookback windows from Condor, evaluates rules,
  and persists incident transitions in ClickHouse.
- **Hummingbird** handles optional outbound cloud reporting. A cloud outage
  does not prevent local ingestion, evaluation or operator access.

k0s supervises each workload using its embedded containerd. Optional capability
processes remain separate and disabled in Community until explicitly enabled.
There is no shared process registry or additional runtime database.

The native Rust console consumes the authenticated local HTTP/SSE API; it does
not connect directly to ClickHouse. Its SCADA reference views have additional
simulator/topology and desktop dependencies described in the console README.
The HTTP intake/readback tutorial can run without those dependencies.

Raw events remain within the chosen installation. Cloud reporting is opt-in
and outbound-only. Local tokens protect intake, operator reads and interservice
calls; the cluster network policy restricts cross-process access.

See [deployment and operation](../deploy/k0s/README.md),
[signal/service contracts](../../../contracts/tucunar-capabilities.openapi.yaml)
and [native console API](../../../contracts/tucunar-cell-console.openapi.yaml).
