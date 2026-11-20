# SCADA reference readers

This optional reference integration reads water, compressed air, electrical equipment and grinding/flotation from
[SCADA Simulator](https://github.com/alonsoburon/scada-simulator) over MQTT,
Modbus TCP, OPC UA and MCP. Each path retains its own scenario, source, tag,
unit and quality. Modbus observations use read time because the register map
has no source timestamp. MCP supplies the water topology, observed equipment inventories and simulator alarms. Water exposes observed connectivity. Electrical network uses the simulator’s explicit common-bus dispatch model: grid import, solar, storage and diesel feed a logical plant bus, with water pumps and concentrator as selected loads. Its provenance is `simulator-model`; it is not a physical wiring survey, and auxiliary site loads are omitted. Air and grinding display observed assets without inferred links.

Start the simulator following its README and keep its operator session active.
These readers target its local unauthenticated development endpoints. Use an
isolated network. Customer equipment requires an explicitly scoped adapter and
its authentication and tag mapping.

From this directory,

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
export SCADA_REGISTER_MAP=/absolute/path/to/scada-simulator/scada_playground/.runtime/modbus_register_map.csv
./start_tucunar_bridge.sh
```

The launcher reads the ingest token from the local k0s secret unless
`SIGNAL_INGEST_TOKEN` is supplied. It starts a loopback Swift forward when
needed. One launcher runs per checkout. A failed reader process restarts with
exponential delays of 1 to 30 seconds. Sixty seconds running resets the delay.
Ctrl+C stops both supervisors, their readers and pending retries.

Endpoint overrides are `SCADA_MQTT_HOST`, `SCADA_MQTT_PORT`, `SCADA_MQTT_TOPIC`,
`SCADA_MODBUS_HOST`, `SCADA_MODBUS_PORT`, `SCADA_OPCUA_URL`,
`SCADA_OPCUA_NAMESPACE`, `SCADA_MCP_URL` and `TUCUNAR_SIGNAL_URL`.
`SCADA_TAG_PREFIX` optionally restricts the four supported catalog areas (empty by default) and `TUCUNAR_POLL_SECONDS`
controls the three polled readers. The Modbus catalog must match the source.

The native console has a system selector for Water, Compressed air, Electrical and Grinding & flotation. Scenario roots are `scada-water`, `scada-air`, `scada-power` and `scada-grinding`. The old generic `scada-electrical` inventory remains available to existing consumers. Each root is MQTT; the other protocols append `-modbus`, `-opcua` or `-mcp`. MCP alarms use `scada-alerts` and are filtered by area in the console. Numeric units, quality and original source timestamps are preserved. A failed system batch does not prevent the remaining systems from publishing.
The private observed topology file defaults to `/tmp/tucunar-scada-topology.json`.
Set `TUCUNAR_TOPOLOGY_FILE` on both reader and console to override it.
Its schema is `contracts/scada-topology.schema.json` at the repository root.

Reader routing and observed inventory checks run without live services:

```bash
.venv/bin/python -m unittest test_readers.py
```
