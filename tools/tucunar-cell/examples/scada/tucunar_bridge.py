"""Forward local SCADA simulator water telemetry to Tucunar Swift."""
import hashlib
import json
import logging
import math
import os
import requests
import paho.mqtt.client as mqtt

TOKEN = os.environ["SIGNAL_INGEST_TOKEN"]
TARGET = os.environ.get("TUCUNAR_SIGNAL_URL", "http://127.0.0.1:18080/v1/signals/batch")
TOPIC = os.environ.get("SCADA_MQTT_TOPIC", "scada/factory/areas/+/telemetry")
MQTT_HOST = os.environ.get("SCADA_MQTT_HOST", "127.0.0.1")
MQTT_PORT = int(os.environ.get("SCADA_MQTT_PORT", "1883"))
SYSTEMS = {"water_supply_network": "water", "compressed_air": "air", "electrical": "electrical", "grinding_and_flotation": "grinding", "grid_generation": "power", "grid_transmission": "power", "grid_distribution": "power", "mine_power_network": "power"}
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")


def on_connect(client, userdata, flags, reason_code, properties):
    if reason_code.is_failure:
        logging.error("MQTT connection failed: %s", reason_code)
        return
    client.subscribe(TOPIC)
    logging.info("Subscribed to %s", TOPIC)


def on_message(client, userdata, message):
    try:
        payload = json.loads(message.payload)
        timestamp = payload["timestamp"]
        signals = []
        area = message.topic.split("/")[-2]
        if area not in SYSTEMS:
            return
        for tag in payload["tags"]:
            value = tag.get("value")
            if isinstance(value, bool) or not isinstance(value, (float, int)) or not math.isfinite(value):
                continue
            tag_id = tag["tag"]
            identity = hashlib.sha256(f"{timestamp}:{tag_id}".encode()).hexdigest()
            signals.append({
                "external_id": identity, "source": "scada-simulator",
                "tenant_id": "tucunar-simulation", "site_id": "scada-factory",
                "entity_id": tag_id.rsplit(".", 1)[0], "kind": "measurement",
                "name": tag["sensor"], "value": value, "unit": tag.get("units", ""),
                "dimensions": {"quality": tag.get("quality", "UNKNOWN"), "protocol": "mqtt",
                               "scenario_id": "scada-" + SYSTEMS[area], "profile": "scada-simulator"},
                "occurred_at": timestamp,
            })
        if signals:
            response = requests.post(TARGET, json={"signals": signals},
                                     headers={"Authorization": f"Bearer {TOKEN}"}, timeout=8)
            response.raise_for_status()
            logging.info("Accepted %d factory telemetry signals", len(signals))
    except (KeyError, ValueError, requests.RequestException) as error:
        logging.error("Telemetry batch failed: %s", error)


if __name__ == "__main__":
    client = mqtt.Client(mqtt.CallbackAPIVersion.VERSION2)
    client.on_connect = on_connect
    client.on_message = on_message
    client.connect(MQTT_HOST, MQTT_PORT, 60)
    client.loop_forever(retry_first_connection=True)
