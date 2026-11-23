#!/usr/bin/env bash
# Prints each base the controller knows of: node, base, state, progress,
# build time, units. Usage: HOTBED_CONTROLLER=host scripts/bases.sh
set -euo pipefail
curl -sf -X POST "http://${HOTBED_CONTROLLER:?set HOTBED_CONTROLLER}:7120/hotbed.v1.ClusterService/GetSnapshot" \
  -H 'Content-Type: application/json' -d '{}' |
  python3 -c '
import sys, json
d = json.load(sys.stdin)
names = {n["id"]: n["hostname"] for n in d.get("nodes", [])}
for b in d.get("bases", []):
    print(names.get(b["nodeId"], b["nodeId"]), b["id"], b["state"].removeprefix("BASE_STATE_").lower(),
          "%.0f%%" % (100 * b.get("progress", 0)), b.get("buildSeconds", "-"), b.get("units", "-"))'
