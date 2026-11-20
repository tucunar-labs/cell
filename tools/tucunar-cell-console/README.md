# Tucunar cell console

Native operator console for the Tucunar cell. Rust + [Raven](https://github.com/oscar-soirey/Raven)
(no webview, no Electron): one binary that talks to the local Cell API over
`127.0.0.1:18080` and renders the SCADA simulator's water network, compressed air, electrical equipment and grinding/flotation.

## Two front ends, one contract

- `target/release/tucunar-cell-console` — the native Raven build. It consumes
  SSE and `/api/console/events` from the cell, with a selector for MQTT,
  Modbus TCP, OPC UA and MCP. The topology and active water alarms are
  supplied by the simulator's MCP bridge.

## Data source

Run the SCADA simulator and its Tucunar bridges first. Each source is ingested
under a separate scenario (`scada-water`, `scada-water-modbus`,
`scada-water-opcua`, `scada-water-mcp`). The bridge writes the observed topology
to `/tmp/tucunar-scada-topology.json`; its contract is
`../../contracts/scada-topology.schema.json`. Topology expires after 60 seconds.
The signal API exposes each reading's quality; stale or bad readings are not
presented as healthy flow.

The system selector keeps each area and protocol in a separate signal scenario. Water retains its observed network. Electrical network shows the simulator’s common-bus dispatch dependencies, with live MW, charging direction and water/concentrator feeders; these are labeled model relationships, not physical wiring. Air and grinding expose observed equipment without inferred connections. Selecting equipment updates its trends, statistics and inspector. Metric panels use available sensors on that equipment.

## Screens

- **Grid** — observed asset inventory, topology, flows, tank levels, water alarms and four trend charts. Select an asset in the inventory or map to highlight it on the map and inspect its readings, units, quality and connected objects.
- **Analysis** — recent flow history, statistics and source-quality findings.

The water reference renders reservoirs, cylindrical water tanks, reclaim ponds and concentrator sumps as flat 2D vessels with rounded shells, inset water and subtle shading in both the map and inventory. Water fill follows each asset’s measured `tank_level` (0–100%), with a 450 ms smoothing time constant between updates. Missing readings show `?`; stale values retain their explicit quality label. The mapping uses the reference topology IDs; unrecognized assets retain a generic fallback.

## Running it

The console reads the local Cell API and the topology snapshot.

```bash
cargo build --release --locked

./target/release/tucunar-cell-console
```

Install the shared Linux x64 library from Raven **v0.2.2** using its upstream
release and verify the archive against the upstream `SHA256SUMS.txt` before
installing it. `build.rs` uses `/usr/local/lib` by default. Set `RAVEN_LIB_DIR`
to override the directory at build time. Raven needs a Vulkan-capable desktop
and its upstream platform dependencies. The console source is Apache-2.0.

For a per-user Linux amd64 library installation, use the source distribution's
pinned downloader. It verifies the archive SHA-256 from `community-release.json`,
extracts only the shared library and its notices, and creates the linker name:

```bash
python3 ../tucunar-cell/scripts/community-tools.py "$HOME/.local/share/tucunar-cell-console/raven-v0.2.2" --raven
RAVEN_LIB_DIR="$HOME/.local/share/tucunar-cell-console/raven-v0.2.2" cargo build --release --locked
```

Keep the upstream LICENSE and NOTICE with the extracted library. On a minimal
Debian desktop, install `libvulkan1`, `mesa-vulkan-drivers`, `libx11-6`,
`libx11-xcb1`, `libxcb1`, `libxkbcommon0`, `libwayland-client0`,
`libwayland-cursor0` and `libdecor-0-0` using the system package manager.
`--check` verifies API access without creating a graphics window; it does not
verify Vulkan presentation. Plain Xvfb lacks the DRI3 presentation required by
Raven's Vulkan X11 backend; use a Vulkan-capable desktop or an isolated nested
Wayland compositor for visual tests. On Debian with Mesa's software Vulkan
driver installed, `VK_DRIVER_FILES=/usr/share/vulkan/icd.d/lvp_icd.json` selects
Lavapipe explicitly when troubleshooting multi-driver systems. Software rendering
is a diagnostic option and does not establish performance on customer GPUs.

For a protected cell, configure the private operator token before starting,

```bash
export TUCUNAR_CONSOLE_TOKEN_FILE="$HOME/.local/share/tucunar-cell/operator.token"
./target/release/tucunar-cell-console --check
./target/release/tucunar-cell-console
```

`--check` exercises API access without opening a window. Both JSON reads and
the live SSE connection send the configured operator token. The token file
must have mode 600. `CANARY_NODE_API_TOKEN` is an alternative environment
setting. The file takes precedence when both are present.

The API is expected behind a port-forward:

```bash
/usr/local/bin/k0s kubectl -n tucunar-cell port-forward --address=127.0.0.1 \
  service/swift 18080:8080
```

## Desktop entry

```bash
./install.sh                 # build (release) + install the menu entry and icon
./install.sh --no-build      # install what is already in target/release
./install.sh --uninstall
```

It installs `tucunar-cell-console.desktop` in `~/.local/share/applications` and
`tucunar-water-grid.svg` under the hicolor theme, both pointing at
`dist/tucunar-cell-console-run`. The launcher resolves the checkout, automatically uses the private
`~/.local/share/tucunar-cell/operator.token` created by Community initialization
when no token setting is supplied, and starts
the `tucunar-cell-api-forward` user unit when it is installed and the API port
is closed, then executes the release binary.

## Environment

| Variable | Purpose |
|---|---|
| `TUCUNAR_CELL_ADDR` | Loopback IP and port, default `127.0.0.1:18080`. Use a local forward for remote cells. |
| `TUCUNAR_CONSOLE_TOKEN_FILE` | Private operator token file for JSON and SSE reads. |
| `CANARY_NODE_API_TOKEN` | Operator token when a token file is absent. |
| `TUCUNAR_TOPOLOGY_FILE` | Optional path to the local observed topology snapshot. |
| `TUCUNAR_CONSOLE_VIEW` | Start on a screen: `0` grid, `1` analysis. |
| `TUCUNAR_CONSOLE_DEBUG` | Log refreshes, apply timings and widget rects to stderr. |
| `TUCUNAR_CONSOLE_DEBUG_INPUT` | Log mouse button events and their target widget. |

## Notes

- All network work happens on worker threads (`cell-api` for reads, `cell-stream`
  for the event stream); the UI only applies results and repaints.
- The first read loads up to 12,000 rows from the past 15 minutes. At high
  signal rates that covers a shorter interval. Each SSE signal event then
  fetches rows since the latest timestamp (with a 2 s overlap and deduplication).
- Charts and the map are custom-painted widgets (`Rcustom_set_paint_callback`),
  so there is no layout churn when values change.
- The map redraws at 20 fps for moving flow markers between data updates; the
  live badge reflects source freshness (5 s for MQTT, 12 s for the polled sources). New readings
  ease into the chart and map over 600 ms. Switching screens still re-renders
  immediately from the current snapshot.
