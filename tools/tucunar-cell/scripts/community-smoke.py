#!/usr/bin/env python3
"""Verify protected intake and persistent readback through a local Swift API."""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import time
from urllib.error import HTTPError
from urllib.parse import urlparse
from urllib.request import Request, urlopen
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--secrets", required=True, type=Path)
    parser.add_argument("--url", default="http://127.0.0.1:18080")
    parser.add_argument("--incident", action="store_true", help="exercise the bundled Community rule (about 90 seconds)")
    args = parser.parse_args()
    parsed = urlparse(args.url)
    if parsed.hostname not in ("127.0.0.1", "localhost", "::1") or parsed.scheme != "http" or parsed.username or parsed.query or parsed.fragment or parsed.path not in ("", "/"):
        parser.error("url must be a loopback HTTP origin")
    credentials = dict(line.split("=", 1) for line in args.secrets.read_text().splitlines()
                       if line and not line.startswith("#"))
    base = args.url.rstrip("/")

    def request(path, token="", payload=None):
        headers = {"Authorization": f"Bearer {token}"} if token else {}
        data = None
        if payload is not None:
            data = json.dumps(payload).encode()
            headers["Content-Type"] = "application/json"
        with urlopen(Request(base + path, data=data, headers=headers), timeout=10) as response:
            return response.status, response.read()

    request("/readyz")
    scenario = "community-smoke-" + uuid.uuid4().hex
    payload = {"source": "community-smoke", "tenant_id": scenario, "entity_id": "test-sensor",
               "kind": "measurement", "name": "temperature", "value": 85, "unit": "C",
               "dimensions": {"scenario_id": scenario, "quality": "GOOD"},
               "occurred_at": datetime.now(timezone.utc).isoformat(), "external_id": scenario}
    for path, body in (("/api/console/events", None), ("/api/stream", None), ("/v1/signals", payload)):
        try:
            request(path, payload=body)
        except HTTPError as error:
            if error.code != 401:
                raise
        else:
            raise RuntimeError(f"Unauthenticated request was accepted at {path}")
    status, _ = request("/v1/signals", credentials["SIGNAL_INGEST_TOKEN"], payload)
    if status != 202:
        raise RuntimeError("Signal was not accepted")
    for _ in range(20):
        _, body = request(f"/api/console/events?scenario={scenario}&window_seconds=60", credentials["CANARY_NODE_API_TOKEN"])
        events = json.loads(body)["events"]
        if any(row["entity_id"] == "test-sensor" and row["value"] == 85 for row in events):
            print(f"PASS readiness, protected reads/SSE/intake, accepted signal and persistent readback ({scenario})")
            if args.incident:
                exercise_incident(request, credentials, payload, scenario)
            return
        time.sleep(1)
    raise RuntimeError("Accepted signal was not visible in persistent readback within 20s")


def exercise_incident(request, credentials, payload, scenario):
    """Observe an active incident and its disappearance after healthy lookbacks."""
    token = credentials["CANARY_NODE_API_TOKEN"]

    def active():
        _, body = request("/api/overview", token)
        return any(row["tenant_id"] == scenario and row["source"] == "community-smoke"
                   for row in json.loads(body)["incidents"])

    # Rules require at least three samples; a single stored reading is not
    # enough to trigger an incident. Use distinct external IDs for deduplication.
    for _ in range(2):
        hot = dict(payload, external_id=uuid.uuid4().hex,
                   occurred_at=datetime.now(timezone.utc).isoformat())
        request("/v1/signals", credentials["SIGNAL_INGEST_TOKEN"], hot)
    print("Waiting for community.temperature.high (bundled rule required)...", flush=True)
    deadline = time.monotonic() + 40
    while not active():
        if time.monotonic() >= deadline:
            raise RuntimeError("No active incident within 40s; check Community rules and evaluator logs")
        time.sleep(2)
    print("PASS active incident observed; sending healthy readings while the 60s max window expires", flush=True)
    deadline = time.monotonic() + 100
    while time.monotonic() < deadline:
        healthy = dict(payload, value=25, external_id=uuid.uuid4().hex,
                       occurred_at=datetime.now(timezone.utc).isoformat())
        request("/v1/signals", credentials["SIGNAL_INGEST_TOKEN"], healthy)
        time.sleep(2)
        if not active():
            print("PASS incident became inactive after healthy readings; synthetic evidence is retained")
            return
    raise RuntimeError("Incident remained active after 100s; inspect evaluator logs")


if __name__ == "__main__":
    main()
