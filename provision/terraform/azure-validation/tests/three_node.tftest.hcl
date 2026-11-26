# The block R4 profile (three_node): node-1..node-3, client role, private
# addresses .11-.13, exclusive with the ten-node cluster. No real provider.
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
  subscription_id            = "11111111-1111-1111-1111-111111111111"
  resource_group_name        = "test"
  location                   = "westus2"
  ssh_public_key             = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKipVtAhJcCsY55aFCnXJhgbsCM2HTDIOF2GgvLbAVcg test-fixture"
  operator_cidr              = "192.0.2.1/32"
  image_version              = "24.04.202610210"
  artifact_manifest_sha256   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  vm_size                    = "Standard_D2s_v5"
  process_cleanup_reserve_ms = 15000
  run_declaration            = { work_deadline = "2030-01-01T18:00:00Z", hard_deadline = "2030-01-01T18:30:00Z", cloud_cleanup_reserve_seconds = 1200, budget_usd = 3, abort_estimate_usd = 2.5 }
}

run "three_nodes" {
  command = apply
  variables {
    three_node = true
  }
  assert {
    condition     = [for h in jsondecode(output.inventory_json).hosts : h.id] == ["node-1", "node-2", "node-3"]
    error_message = "The profile is exactly node-1..node-3."
  }
  assert {
    condition     = [for h in jsondecode(output.inventory_json).hosts : h.data.address] == ["10.84.1.11", "10.84.1.12", "10.84.1.13"] && alltrue([for h in jsondecode(output.inventory_json).hosts : h.roles == ["client"] && length(h.services) == 0 && length(h.disks) == 0])
    error_message = "Three client nodes at .11-.13, no HTTP services, no cache disk."
  }
  assert {
    condition     = jsondecode(output.inventory_json).run_id == "r4-three-gate" && length(output.os_disk_ids) == 3
    error_message = "One OS disk per node, under the profile's run id."
  }
}

run "three_and_cluster_refused" {
  command = plan
  variables {
    three_node = true
    cluster    = true
  }
  expect_failures = [var.three_node]
}
