"""CPU-only regression of the production inventory iteration and resource joins.
Run explicitly with python3 partial_inventory.py; needs local terraform, no providers.
"""
import pathlib, re, subprocess, tempfile


def main():
    source = (pathlib.Path(__file__).parents[1] / "azure/outputs.tf").read_text()
    iteration = re.search(r"hosts = \[for id in (.+) : \{", source).group(1)
    vm = re.search(r"resource_id = (.+)", source).group(1).strip()
    ip = re.search(r"address        = (.+)", source).group(1).strip()
    expression = "[for id in " + iteration + " : {resource_id = " + vm + ", address = " + ip + "}]"
    expression = expression.replace("var.nodes", "local.nodes").replace("azurerm_linux_virtual_machine.node", "local.vms").replace("azurerm_public_ip.node", "local.ips")
    for name, vms, ips, expected in [
        ("complete", '{a={id="vm-a"},b={id="vm-b"}}', '{a={ip_address="192.0.2.1"},b={ip_address="192.0.2.2"}}', '"vm-b"'),
        ("destroyed-ip", '{a={id="vm-a"},b={id="vm-b"}}', '{a={ip_address="192.0.2.1"}}', '"vm-a"'),
        ("destroyed-vm", '{a={id="vm-a"}}', '{a={ip_address="192.0.2.1"},b={ip_address="192.0.2.2"}}', '"vm-a"'),
        ("empty", '{}', '{}', '[]'),
    ]:
        with tempfile.TemporaryDirectory(prefix="partial-inventory-") as directory:
            pathlib.Path(directory, "main.tf").write_text('locals {\n nodes={a={},b={}}\n vms='+vms+'\n ips='+ips+'\n}\n')
            result = subprocess.run(["terraform", "console", "-no-color"], cwd=directory, input=expression+"\n", text=True, capture_output=True, timeout=30)
            assert result.returncode == 0, (name, result.stderr)
            assert expected in result.stdout, (name, result.stdout)
            if name not in ("complete", "empty"):
                assert '"vm-b"' not in result.stdout, (name, result.stdout)
            print(name + ": PASS")


if __name__ == "__main__":
    main()
