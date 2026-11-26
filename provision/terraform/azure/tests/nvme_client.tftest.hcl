# The block R4 profile's module inputs: internal_ports (subnet-only) and
# nvme_client (nvme-cli, native NVMe multipath, nvme_tcp at boot). Off by
# default, so no other profile changes. No real provider instance.
mock_provider "azurerm" {
  mock_resource "azurerm_virtual_network" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Network/virtualNetworks/smoke" } }
  mock_resource "azurerm_subnet" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Network/virtualNetworks/smoke/subnets/nodes" } }
  mock_resource "azurerm_network_security_group" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Network/networkSecurityGroups/smoke" } }
  mock_resource "azurerm_public_ip" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Network/publicIPAddresses/smoke", ip_address = "192.0.2.10" } }
  mock_resource "azurerm_network_interface" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Network/networkInterfaces/smoke" } }
  mock_resource "azurerm_linux_virtual_machine" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Compute/virtualMachines/smoke", os_disk = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Compute/disks/os" } } }
  mock_resource "azurerm_managed_disk" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Compute/disks/cache" } }
}
variables {
  resource_group_name        = "test"
  location                   = "westus2"
  run_id                     = "mock-r4"
  artifact_manifest_sha256   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  operator_cidr              = "192.0.2.1/32"
  ssh_public_key             = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKipVtAhJcCsY55aFCnXJhgbsCM2HTDIOF2GgvLbAVcg test-fixture"
  image_version              = "24.04.202610210"
  process_cleanup_reserve_ms = 15000
  run_declaration = {
    work_deadline                 = "2030-01-01T01:00:00Z"
    hard_deadline                 = "2030-01-01T02:00:00Z"
    cloud_cleanup_reserve_seconds = 1200
    budget_usd                    = 3
    abort_estimate_usd            = 2.5
  }
  nodes = {
    node-1 = { roles = ["client"], size = "Standard_D2s_v5", private_ip = "10.84.1.11", cache_disk_gib = 0, format_allowed = false, services = [] }
    node-2 = { roles = ["client"], size = "Standard_D2s_v5", private_ip = "10.84.1.12", cache_disk_gib = 0, format_allowed = false, services = [] }
    node-3 = { roles = ["client"], size = "Standard_D2s_v5", private_ip = "10.84.1.13", cache_disk_gib = 0, format_allowed = false, services = [] }
  }
  node_host_keys = {
    node-1 = { ed25519_private = "PRIV-1", ed25519_public = "ssh-ed25519 PUB-node-1" }
    node-2 = { ed25519_private = "PRIV-2", ed25519_public = "ssh-ed25519 PUB-node-2" }
    node-3 = { ed25519_private = "PRIV-3", ed25519_public = "ssh-ed25519 PUB-node-3" }
  }
}

run "block_profile" {
  command = apply
  variables {
    internal_ports = [19600, 19601, 19602, 4420]
    nvme_client    = true
  }
  assert {
    condition = alltrue([for r in azurerm_network_security_group.run.security_rule : r.name != "internal-services" || (
      toset(r.destination_port_ranges) == toset(["19600", "19601", "19602", "4420"]) &&
    r.source_address_prefix == var.subnet_cidr && r.destination_address_prefix == var.subnet_cidr)]) && length([for r in azurerm_network_security_group.run.security_rule : r if r.name == "internal-services"]) == 1
    error_message = "The block ports must open between the run's own subnet only, exactly these four."
  }
  assert {
    condition     = alltrue([for r in azurerm_network_security_group.run.security_rule : r.name == "controller-ssh" ? r.source_address_prefix == var.operator_cidr && r.destination_port_range == "22" : r.name != "internal-services" || !contains(r.destination_port_ranges, "22")])
    error_message = "SSH 22 stays controller-only."
  }
  assert {
    condition = alltrue([for id, vm in azurerm_linux_virtual_machine.node :
      strcontains(base64decode(vm.custom_data), "nvme-cli") &&
      strcontains(base64decode(vm.custom_data), "options nvme_core multipath=Y") &&
      strcontains(base64decode(vm.custom_data), "/etc/modules-load.d/seaweed-nvme-tcp.conf") &&
      strcontains(base64decode(vm.custom_data), "modprobe") &&
      strcontains(base64decode(vm.custom_data), "PUB-${id}") &&
      startswith(base64decode(vm.custom_data), "#cloud-config\n")
    ])
    error_message = "Every node's cloud-init carries the NVMe/TCP client prerequisites and its own pinned server key."
  }
  assert {
    condition     = [for h in jsondecode(output.inventory_json).hosts : h.id] == ["node-1", "node-2", "node-3"]
    error_message = "The inventory names exactly node-1..node-3."
  }
}

# Off by default: no NVMe client in cloud-init, no extra port, and a node
# without a pinned key gets the module's file byte for byte.
run "default_profile_unchanged" {
  command = apply
  variables {
    node_host_keys = {}
    nodes = { smoke = { roles = ["client"], size = "Standard_D2s_v5", private_ip = "10.84.1.10", cache_disk_gib = 0, format_allowed = false, services = [] } }
  }
  assert {
    condition     = base64decode(azurerm_linux_virtual_machine.node["smoke"].custom_data) == file("${path.module}/cloud-init.yaml")
    error_message = "Without nvme_client the cloud-init is the module's file, unchanged."
  }
  assert {
    condition     = length([for r in azurerm_network_security_group.run.security_rule : r if r.name == "internal-services"]) == 0
    error_message = "No service and no internal port opens nothing."
  }
}

run "default_with_a_pinned_key_has_no_nvme" {
  command = apply
  assert {
    condition     = alltrue([for vm in azurerm_linux_virtual_machine.node : !strcontains(base64decode(vm.custom_data), "nvme") && strcontains(base64decode(vm.custom_data), "ssh_keys")])
    error_message = "A pinned key alone adds no NVMe client prerequisite."
  }
}

run "internal_port_22_refused" {
  command = plan
  variables {
    internal_ports = [19600, 22]
  }
  expect_failures = [var.internal_ports]
}

run "internal_port_duplicate_refused" {
  command = plan
  variables {
    internal_ports = [4420, 4420]
  }
  expect_failures = [var.internal_ports]
}
