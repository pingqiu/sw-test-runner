# No real provider instance: schema-backed values are entirely mocked.
mock_provider "azurerm" {
  mock_resource "azurerm_resource_group" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-claude04-standing-20261120" } }
  mock_resource "azurerm_virtual_network" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-claude04-standing-20261120/providers/Microsoft.Network/virtualNetworks/box" } }
  mock_resource "azurerm_subnet" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-claude04-standing-20261120/providers/Microsoft.Network/virtualNetworks/box/subnets/box" } }
  mock_resource "azurerm_network_security_group" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-claude04-standing-20261120/providers/Microsoft.Network/networkSecurityGroups/box" } }
  mock_resource "azurerm_public_ip" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-claude04-standing-20261120/providers/Microsoft.Network/publicIPAddresses/box", ip_address = "192.0.2.20" } }
  mock_resource "azurerm_network_interface" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-claude04-standing-20261120/providers/Microsoft.Network/networkInterfaces/box" } }
  mock_resource "azurerm_linux_virtual_machine" { defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-claude04-standing-20261120/providers/Microsoft.Compute/virtualMachines/box", os_disk = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-claude04-standing-20261120/providers/Microsoft.Compute/disks/os" } } }
}
variables {
  subscription_id     = "00000000-0000-0000-0000-000000000000"
  resource_group_name = "rg-claude04-standing-20261120"
  name                = "c04-standing"
  operator_cidr       = "192.0.2.1/32"
  ssh_public_key      = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKipVtAhJcCsY55aFCnXJhgbsCM2HTDIOF2GgvLbAVcg test-fixture"
  image_version       = "24.04.202610210"
}
run "spot_box_shape" {
  command = apply
  assert {
    condition     = azurerm_linux_virtual_machine.box.priority == "Spot" && azurerm_linux_virtual_machine.box.eviction_policy == "Deallocate" && azurerm_linux_virtual_machine.box.max_bid_price == -1
    error_message = "the box must be Spot, evicted to Deallocated, max price -1"
  }
  assert {
    condition     = azurerm_linux_virtual_machine.box.size == "Standard_D32ads_v5" && azurerm_linux_virtual_machine.box.os_disk[0].disk_size_gb == 256
    error_message = "plan r0 size / OS disk"
  }
  assert {
    condition     = length(azurerm_linux_virtual_machine.box.identity) == 0
    error_message = "the box must hold no Azure identity (plan r0: no managed identity)"
  }
  assert {
    condition = alltrue([for r in [one([for r in azurerm_network_security_group.box.security_rule : r if r.access == "Allow"])] :
      r.direction == "Inbound" && r.protocol == "Tcp" && r.destination_port_range == "22" && length(coalesce(r.destination_port_ranges, [])) == 0 &&
    r.source_address_prefix == "192.0.2.1/32" && r.priority == 100])
    error_message = "exactly one allow rule: inbound TCP 22 only, from the operator /32 only"
  }
  assert {
    condition = length([for r in azurerm_network_security_group.box.security_rule : r if r.access == "Deny" && r.direction == "Inbound" &&
      r.protocol == "*" && r.source_port_range == "*" && r.destination_port_range == "*" && r.source_address_prefix == "*" && r.destination_address_prefix == "*" &&
    r.priority > 100]) == 1 && length(azurerm_network_security_group.box.security_rule) == 2
    error_message = "a deny-all inbound rule after the SSH allow, and no other rule"
  }
  assert {
    condition     = output.box.public_ip == "192.0.2.20" && output.box.resource_group_name == "rg-claude04-standing-20261120"
    error_message = "output carries the address and RG"
  }
  assert {
    condition     = strcontains(base64decode(azurerm_linux_virtual_machine.box.custom_data), "-ge $(( 60 * 60 ))") && strcontains(base64decode(azurerm_linux_virtual_machine.box.custom_data), "run=$${STANDING_RUN_DIR:-/run/standing}")
    error_message = "idle guard threshold rendered from idle_minutes; per-boot clock under /run"
  }
}
run "existing_rg" {
  command = apply
  variables { create_resource_group = false }
  assert {
    condition     = length(azurerm_resource_group.box) == 0 && azurerm_linux_virtual_machine.box.resource_group_name == "rg-claude04-standing-20261120"
    error_message = "create_resource_group = false must use the given RG and create none"
  }
}
run "rejects_open_ssh" {
  command = plan
  variables { operator_cidr = "0.0.0.0/0" }
  expect_failures = [var.operator_cidr]
}
run "rejects_shared_rg" {
  command = plan
  variables { resource_group_name = "rg-codex02-azure-test-20261029" }
  expect_failures = [var.resource_group_name]
}
run "on_demand" {
  command = apply
  variables { priority = "Regular" }
  assert {
    condition     = azurerm_linux_virtual_machine.box.priority == "Regular" && azurerm_linux_virtual_machine.box.eviction_policy == null
    error_message = "Regular = on-demand: no eviction policy (and no max price)"
  }
  assert {
    condition     = length(azurerm_linux_virtual_machine.box.identity) == 0 && length(azurerm_network_security_group.box.security_rule) == 2
    error_message = "on-demand keeps the same hardening (no identity, SSH /32 + deny-all only)"
  }
}
run "rejects_unknown_priority" {
  command = plan
  variables { priority = "Low" }
  expect_failures = [var.priority]
}
