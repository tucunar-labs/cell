# Contributing to Tucunar Cell

Start with the first-run guide in `README.md`. The supported baseline is Linux,
k0s with its embedded containerd, four independent Go services, and ClickHouse.
The native Rust console is optional for API development.

## Development checks

From the repository root:

```bash
python3 tools/tucunar-cell/scripts/verify-export.py
```

This checks the actual standalone source distribution and runs Go test, build
and vet for contracts and runtime. Integration tests requiring live services
may skip; passing source CI is not an installation or production certification.
For Rust changes, also run:

```bash
cd tools/tucunar-cell-console
cargo test --locked
cargo build --release --locked
```

Install Raven as documented in the console README before building its desktop
binary. Keep Go and Rust dependencies separate. Preserve compatibility within
the same major and update versioned contracts when component interfaces change.
Do not add another runtime database or join the independent services together.

## Issues and pull requests

Report bugs with version, architecture, installation method, exact command,
expected result, actual result and redacted diagnostics. Report vulnerabilities
privately using `SECURITY.md`. For a proposal, explain the concrete operator
problem and a small example before proposing a new connector or capability.

Keep pull requests focused. Describe behavior changed and commands actually
run, including skipped integrations or unavailable desktop dependencies. Add
meaningful regression coverage for behavior changes. Never include tokens,
rendered Secrets, customer payloads or internal operational material.

The public source is exported from Tucunar's private workspace using an explicit
allowlist. Maintainers integrate accepted public patches into that canonical
source and regenerate the distribution; private application code and Git history
are not imported into this repository. Changes to exported files must be made
in the canonical source as well so the next export preserves them. Public issues
and pull requests are the contribution channel; no response SLA is promised.

Contributions are submitted under Apache-2.0. Preserve copyright and dependency
notices. The project name and logos remain subject to trademark rights.
