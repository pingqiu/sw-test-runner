"""Inspect `terraform test -json -verbose` mock plan/state; no account calls."""
import json
import sys


def resources(module):
    yield from module.get("resources", [])
    for child in module.get("child_modules", []):
        yield from resources(child)


events = [json.loads(line) for line in sys.stdin if line.strip()]
snapshots = [event[event["type"]] for event in events
             if event.get("type") in ("test_plan", "test_state")]
assert snapshots, "Missing verbose mock plan/state"
for snapshot in snapshots:
    if "resource_changes" in snapshot:
        items = [dict(r, values=r["change"]["after"])
                 for r in snapshot["resource_changes"] if r["mode"] == "managed"]
    else:
        values = snapshot.get("values", snapshot)
        items = list(resources(values["root_module"]))
    assert items and any(r["type"] == "azurerm_linux_virtual_machine" for r in items)
    for item in items:
        assert "dev_test" not in item["type"].lower(), item["address"]
        assert "microsoft.devtestlab" not in str(item["values"]).lower(), item["address"]
        tags = item["values"].get("tags")
        if tags is not None:
            assert tags.get("expires_at") == "2030-01-01T18:30:00Z", item["address"]
assert any(e.get("type") == "test_summary" and e["test_summary"]["status"] == "pass"
           for e in events), "Mock test failed"
print("PASS: one-VM mock plan has no DevTestLab resource; expiry tags retained")
