"""Read the SCADA water tags through Modbus, OPC UA and MCP into Swift.

The MQTT path is in tucunar_bridge.py. Each protocol keeps a separate source
and scenario so values and quality can be compared without merging readings.
"""
import asyncio
import csv
import hashlib
import json
import logging
import math
import os
import struct
import tempfile
from datetime import datetime, timezone
from pathlib import Path

import requests
from asyncua import Client
from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client
from pymodbus.client import ModbusTcpClient

ROOT = Path(__file__).resolve().parent
TOKEN = os.environ["SIGNAL_INGEST_TOKEN"]
TARGET = os.environ.get("TUCUNAR_SIGNAL_URL", "http://127.0.0.1:18080/v1/signals/batch")
INTERVAL = float(os.environ.get("TUCUNAR_POLL_SECONDS", "5"))
NAMESPACE = os.environ.get("SCADA_OPCUA_NAMESPACE", "https://scada-learning-lab.local/factory")
OPCUA_URL = os.environ.get("SCADA_OPCUA_URL", "opc.tcp://127.0.0.1:4840/factory/server/")
MCP_URL = os.environ.get("SCADA_MCP_URL", "http://127.0.0.1:8503/mcp")
MODBUS_HOST = os.environ.get("SCADA_MODBUS_HOST", "127.0.0.1")
MODBUS_PORT = int(os.environ.get("SCADA_MODBUS_PORT", "1502"))
CATALOG = Path(os.environ.get("SCADA_REGISTER_MAP", str(ROOT / "modbus_register_map.csv")))
TOPOLOGY_PATH = Path(os.environ.get("TUCUNAR_TOPOLOGY_FILE", "/tmp/tucunar-scada-topology.json"))
TAG_PREFIX = os.environ.get("SCADA_TAG_PREFIX", "")
SYSTEMS = {"water_supply_network": "water", "compressed_air": "air", "electrical": "electrical", "grinding_and_flotation": "grinding", "grid_generation": "power", "grid_transmission": "power", "grid_distribution": "power", "mine_power_network": "power"}
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")


def catalog():
    with CATALOG.open(newline="", encoding="utf-8") as file:
        return [row for row in csv.DictReader(file) if row["tag"].split(".", 1)[0] in SYSTEMS and row["tag"].startswith(TAG_PREFIX)]


def publish(protocol, timestamp, values):
    signals = []
    for reading in values:
        tag, value, units, quality = reading[:4]
        observed_at = reading[4] if len(reading) > 4 else timestamp
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
            continue
        identity = hashlib.sha256(f"{protocol}:{observed_at}:{tag}".encode()).hexdigest()
        entity, name = tag.rsplit(".", 1)
        signals.append({
            "external_id": identity, "source": "scada-" + protocol,
            "tenant_id": "tucunar-simulation", "site_id": "scada-factory",
            "entity_id": entity, "kind": "measurement", "name": name,
            "value": value, "unit": units,
            "dimensions": {"quality": str(quality), "protocol": protocol,
                           "scenario_id": "scada-" + SYSTEMS[tag.split(".", 1)[0]] + "-" + protocol, "profile": "scada-simulator"},
            "occurred_at": observed_at,
        })
    if not signals:
        raise RuntimeError(f"{protocol}: no numeric readings")
    # Bound batches to one system so each console window remains independent.
    errors = []
    for system in dict.fromkeys(SYSTEMS.values()):
        group = [s for s in signals if s["dimensions"]["scenario_id"] == f"scada-{system}-{protocol}"]
        for start in range(0, len(group), 1000):
            try:
                response = requests.post(TARGET, json={"signals": group[start:start + 1000]},
                                         headers={"Authorization": f"Bearer {TOKEN}"}, timeout=10)
                response.raise_for_status()
            except requests.RequestException as error:
                errors.append(f"{system}: {error}")
    if errors:
        raise RuntimeError("; ".join(errors))
    logging.info("%s: accepted %d factory signals", protocol, len(signals))


def read_modbus(rows):
    client = ModbusTcpClient(MODBUS_HOST, port=MODBUS_PORT, timeout=4)
    if not client.connect():
        raise ConnectionError("Modbus connection failed")
    try:
        groups = []
        for row in sorted(rows, key=lambda row: int(row["value_float32_high_register"])):
            address = int(row["value_float32_high_register"])
            if (not groups or address != int(groups[-1][1][-1]["quality_register"]) + 1
                    or address + 3 - groups[-1][0] > 123):
                groups.append((address, []))
            groups[-1][1].append(row)
        values = []
        for first, group in groups:
            last = int(group[-1]["quality_register"])
            response = client.read_input_registers(first, count=last - first + 1, device_id=1)
            if response.isError():
                raise RuntimeError(f"Modbus read failed at {first}: {response}")
            for row in group:
                offset = int(row["value_float32_high_register"]) - first
                high, low, quality = response.registers[offset:offset + 3]
                value = struct.unpack(">f", struct.pack(">HH", high, low))[0]
                values.append((row["tag"], value, row["units"], {0: "GOOD", 1: "STALE", 2: "BAD"}.get(quality, "UNKNOWN")))
        return values
    finally:
        client.close()


async def read_opcua(rows):
    async with Client(OPCUA_URL, timeout=5) as client:
        ns = await client.get_namespace_index(NAMESPACE)
        nodes = [client.get_node(f"ns={ns};s={field}/{row['tag']}")
                 for row in rows for field in ("Value", "Quality", "Units")]
        readings = []
        for start in range(0, len(nodes), 200):
            readings.extend(await client.read_values(nodes[start:start + 200]))
        return [(row["tag"], readings[i * 3], readings[i * 3 + 2], readings[i * 3 + 1])
                for i, row in enumerate(rows)]


async def read_mcp(rows):
    async with streamablehttp_client(MCP_URL) as (reader, writer, _):
        async with ClientSession(reader, writer) as session:
            await session.initialize()
            values = []
            timestamp = None
            observed = []
            for start in range(0, len(rows), 200):
                result = await session.call_tool("read_tags", {"tag_names": [row["tag"] for row in rows[start:start + 200]]})
                if result.isError or not result.structuredContent:
                    raise RuntimeError(f"MCP read failed: {result.content}")
                data = result.structuredContent
                observed.extend(data["values"])
                timestamp = data["snapshot_timestamp"]
                if data.get("not_found"):
                    logging.warning("MCP missing %d tags", len(data["not_found"]))
                values.extend((x["tag"], x["value"], x["units"], x["quality"], x["timestamp"]) for x in data["values"])
            alarms = await session.call_tool("get_alarms", {"limit": 500})
            if alarms.isError or not alarms.structuredContent:
                raise RuntimeError("MCP alarms could not be read")
            topology = await session.call_tool("get_water_network_state", {})
            if topology.isError or not topology.structuredContent:
                raise RuntimeError("MCP water topology could not be read")
            write_topology(topology.structuredContent, observed)
            return timestamp, values, alarms.structuredContent["alarms"]


def power_topology(observed):
    """Adapter mapping of MinePowerNetwork's common-bus dispatch model.

    This is a model relationship, not inferred physical wiring. Only endpoints
    actually present in the MCP readings are included. Site auxiliary loads are
    deliberately omitted; the displayed feeders are water and concentrator.
    """
    assets = {tag["tag"].rsplit(".", 1)[0] for tag in observed}
    specifications = [
        ("grid_transmission.intertie", "Grid import", "source", "active_power"),
        ("grid_generation.solar_farm", "Solar farm", "source", "active_power"),
        ("grid_generation.battery_storage", "Battery storage", "storage", "active_power"),
        ("grid_generation.turbine_generator_01", "Diesel generator 1", "source", "active_power"),
        ("grid_generation.turbine_generator_02", "Diesel generator 2", "source", "active_power"),
        ("mine_power_network.concentrator_substation", "Concentrator", "load", "active_power"),
        ("mine_power_network.water_pump_station", "Water pumps", "load", "pump_load"),
    ]
    nodes, links = [], []
    for entity, label, kind, metric in specifications:
        if entity not in assets:
            continue
        nodes.append({"id": entity, "label": label, "kind": kind, "metric": metric})
        start, end = ("power_network.bus", entity) if kind == "load" else (entity, "power_network.bus")
        links.append({"id": entity, "label": label, "from": start, "to": end,
                      "metric_entity": entity, "metric": metric, "unit": "MW", "kind": "power"})
    if nodes:
        nodes.insert(0, {"id": "power_network.bus", "label": "Plant bus", "kind": "junction"})
    return {"nodes": nodes, "links": links, "provenance": "simulator-model",
            "description": "MinePowerNetwork common-bus dispatch; selected water and concentrator loads, site auxiliaries omitted"}


def write_topology(data, observed):
    topology = {"version": "1.0.0", "source": "scada-mcp", "observed_at": data["snapshot_timestamp"],
                "nodes": [{"id": n["id"], "label": n["label"]} for n in data["nodes"]],
                "links": [{"id": e["id"], "label": e["label"], "from": e["from"], "to": e["to"]} for e in data["links"]]}
    topology["systems"] = {}
    for prefix in SYSTEMS:
        if prefix == "water_supply_network":
            continue
        assets = {}
        for tag in observed:
            if not tag["tag"].startswith(prefix + "."):
                continue
            entity = tag["tag"].rsplit(".", 1)[0]
            assets[entity] = {"id": entity, "label": tag["asset"]}
        topology["systems"][prefix] = {"nodes": list(assets.values()), "links": []}
    topology["systems"]["power_network"] = power_topology(observed)
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", prefix=".tucunar-topology-",
                                     dir=TOPOLOGY_PATH.parent, delete=False) as temporary:
        json.dump(topology, temporary)
        temporary_name = temporary.name
    os.replace(temporary_name, TOPOLOGY_PATH)


def publish_alarms(timestamp, alarms):
    signals = [{"external_id": hashlib.sha256(f"alarm-monitor:{timestamp}".encode()).hexdigest(),
                "source": "scada-mcp-alarms", "tenant_id": "tucunar-simulation", "site_id": "scada-factory",
                "entity_id": "water-alarm-monitor", "kind": "measurement", "name": "alarm_monitor",
                "value": 0, "unit": "", "dimensions": {"scenario_id": "scada-alerts"}, "occurred_at": timestamp}]
    for alarm in alarms:
        if not alarm.get("state", "").upper().startswith("ACTIVE"):
            continue
        message = f"{alarm.get('area', 'Water')}: {alarm.get('message', 'Alarm')}"[:150]
        severity = alarm.get("severity", "UNKNOWN").upper()
        identity = hashlib.sha256(f"alarm:{timestamp}:{message}".encode()).hexdigest()
        signals.append({"external_id": identity, "source": "scada-mcp-alarms", "tenant_id": "tucunar-simulation",
                        "site_id": "scada-factory", "entity_id": message, "kind": "measurement",
                        "name": "alarm_critical" if severity in ("HIGH", "CRITICAL") else "alarm_warning",
                        "value": 1, "unit": "", "dimensions": {"scenario_id": "scada-alerts", "severity": severity},
                        "occurred_at": timestamp})
    if signals:
        response = requests.post(TARGET, json={"signals": signals}, headers={"Authorization": f"Bearer {TOKEN}"}, timeout=10)
        response.raise_for_status()
        logging.info("mcp: accepted %d active water alarms", len(signals) - 1)


async def run_reader(protocol):
    while True:
        try:
            rows = catalog()
            if not rows:
                raise RuntimeError("water tag catalog is empty")
            timestamp = datetime.now(timezone.utc).isoformat()
            if protocol == "modbus":
                values = await asyncio.to_thread(read_modbus, rows)
            elif protocol == "opcua":
                values = await read_opcua(rows)
            else:
                timestamp, values, alarms = await read_mcp(rows)
                await asyncio.to_thread(publish_alarms, timestamp, alarms)
            await asyncio.to_thread(publish, protocol, timestamp, values)
        except Exception as error:
            logging.error("%s: %s", protocol, error)
        await asyncio.sleep(INTERVAL)


async def main():
    await asyncio.gather(*(run_reader(protocol) for protocol in ("modbus", "opcua", "mcp")))


if __name__ == "__main__":
    asyncio.run(main())
