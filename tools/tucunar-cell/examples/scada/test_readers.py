"""Factory routing and inventory contracts; no live endpoints needed."""
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
os.environ.setdefault("SIGNAL_INGEST_TOKEN", "test-only-token")
import tucunar_protocols as protocols
import tucunar_bridge as mqtt


class FactoryReaders(unittest.TestCase):
    def test_protocol_systems_keep_quality_time_and_scenario(self):
        readings = [(f"{prefix}.asset.line_pressure", 42.0, "kPa", "STALE", "2026-11-11T14:00:00Z") for prefix in protocols.SYSTEMS]
        with patch.object(protocols.requests, "post") as post:
            protocols.publish("modbus", "ignored", readings)
        self.assertEqual(post.call_count, 5)
        scenarios = set()
        for call in post.call_args_list:
            signal = call.kwargs["json"]["signals"][0]
            scenarios.add(signal["dimensions"]["scenario_id"])
            self.assertEqual(signal["dimensions"]["quality"], "STALE")
            self.assertEqual(signal["occurred_at"], "2026-11-11T14:00:00Z")
        self.assertEqual(scenarios, {"scada-water-modbus", "scada-air-modbus", "scada-electrical-modbus", "scada-grinding-modbus", "scada-power-modbus"})

    def test_one_failed_system_does_not_block_the_rest(self):
        readings = [(f"{prefix}.asset.pressure", 42.0, "kPa", "GOOD") for prefix in protocols.SYSTEMS]
        with patch.object(protocols.requests, "post") as post:
            post.side_effect = [protocols.requests.Timeout("water unavailable"), unittest.mock.Mock(), unittest.mock.Mock(), unittest.mock.Mock(), unittest.mock.Mock()]
            with self.assertRaisesRegex(RuntimeError, "water unavailable"):
                protocols.publish("opcua", "2026-11-11T14:00:00Z", readings)
            self.assertEqual(post.call_count, 5)

    def test_power_model_connects_only_present_endpoints(self):
        readings = [{"tag": entity + "." + metric} for entity, metric in [
            ("grid_transmission.intertie", "active_power"),
            ("grid_generation.battery_storage", "active_power"),
            ("mine_power_network.water_pump_station", "pump_load")]]
        network = protocols.power_topology(readings)
        self.assertEqual(network["provenance"], "simulator-model")
        self.assertEqual(len(network["nodes"]), 4)
        self.assertEqual(len(network["links"]), 3)
        self.assertTrue(all(link["metric_entity"] in {r["tag"].rsplit(".", 1)[0] for r in readings} for link in network["links"]))
        pump = next(link for link in network["links"] if link["metric"] == "pump_load")
        self.assertEqual(pump["from"], "power_network.bus")
        self.assertEqual(protocols.power_topology([])["nodes"], [])
        self.assertEqual(protocols.power_topology([])["links"], [])

    def test_inventory_uses_observed_assets_without_invented_edges(self):
        observed = [{"tag": "compressed_air.compressor_a.line_pressure", "asset": "Compressor A"}, {"tag": "compressed_air.compressor_a.motor_current", "asset": "Compressor A"}]
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "topology.json"
            with patch.object(protocols, "TOPOLOGY_PATH", target):
                protocols.write_topology({"snapshot_timestamp": "2026-11-11T14:00:00Z", "nodes": [], "links": []}, observed)
            result = json.loads(target.read_text())
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
        self.assertEqual(result["systems"]["compressed_air"], {"nodes": [{"id": "compressed_air.compressor_a", "label": "Compressor A"}], "links": []})

    def test_mqtt_routes_area_and_rejects_unsupported_area(self):
        class Message:
            topic = "scada/factory/areas/electrical/telemetry"
            payload = json.dumps({"timestamp": "2026-11-11T14:00:00Z", "tags": [{"tag": "electrical.transformer_a.active_power", "sensor": "active_power", "value": 12.5, "units": "MW", "quality": "GOOD"}]}).encode()
        with patch.object(mqtt.requests, "post") as post:
            mqtt.on_message(None, None, Message())
            self.assertEqual(post.call_args.kwargs["json"]["signals"][0]["dimensions"]["scenario_id"], "scada-electrical")
            Message.topic = "scada/factory/areas/unsupported/telemetry"
            mqtt.on_message(None, None, Message())
            self.assertEqual(post.call_count, 1)

if __name__ == "__main__":
    unittest.main()
