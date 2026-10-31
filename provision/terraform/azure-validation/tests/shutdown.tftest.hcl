# No real provider instance: schema-backed values are entirely mocked.
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
run "expiry_without_shutdown" {
  command = plan
  assert {
    condition     = output.run_declaration.hard_deadline == var.run_declaration.hard_deadline
    error_message = "The plan must retain the declared expiry deadline."
  }
}
