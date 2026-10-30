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
  resource_group_name        = "test"
  location                   = "westus2"
  run_id                     = "mock-http"
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
    smoke = {
      roles          = ["master", "filer", "volume", "s3", "client"]
      size           = "Standard_D2s_v5"
      private_ip     = "10.84.1.10"
      cache_disk_gib = 4
      format_allowed = false
      services       = [{ name = "master_http", port = 9444 }, { name = "master_grpc", port = 19444 }, { name = "filer_http", port = 8999 }, { name = "filer_grpc", port = 18999 }, { name = "volume_http", port = 8099 }, { name = "volume_grpc", port = 18099 }, { name = "s3_http", port = 8399 }]
    }
  }
}
run "inventory_output" {
  command = apply
  assert {
    condition     = toset([for r in azurerm_network_security_group.run.security_rule : r.destination_port_ranges if r.name == "internal-services"][0]) == toset([for s in var.nodes.smoke.services : tostring(s.port)])
    error_message = "NSG must use the same explicit service ports as inventory, not old defaults."
  }
  assert {
    condition     = jsondecode(output.inventory_json).deadlines == { work = "2030-01-01T01:00:00Z", hard = "2030-01-01T02:00:00Z", cleanup_reserve_ms = 15000 }
    error_message = "Inventory must preserve the declared deadlines and independent process reserve."
  }
  assert {
    condition     = jsonencode(jsondecode(output.inventory_json).hosts[0].services) == jsonencode(var.nodes.smoke.services)
    error_message = "Inventory must carry explicit services unchanged."
  }
}


run "client_only" {
  command = apply
  variables {
    nodes = { smoke = { roles = ["client"], size = "Standard_D2s_v5", private_ip = "10.84.1.10", cache_disk_gib = 0, format_allowed = false, services = [] } }
  }
  assert {
    condition     = length(jsondecode(output.inventory_json).hosts[0].services) == 0 && length([for r in azurerm_network_security_group.run.security_rule : r if r.name == "internal-services"]) == 0
    error_message = "Client-only nodes must preserve explicit [] without an empty NSG service rule."
  }
}
run "zero_process_reserve" {
  command = plan
  variables { process_cleanup_reserve_ms = 0 }
  expect_failures = [var.process_cleanup_reserve_ms]
}
run "over_process_reserve" {
  command = plan
  variables { process_cleanup_reserve_ms = 60001 }
  expect_failures = [var.process_cleanup_reserve_ms]
}
run "fraction_process_reserve" {
  command = plan
  variables { process_cleanup_reserve_ms = 1.5 }
  expect_failures = [var.process_cleanup_reserve_ms]
}
run "reserve_exceeds_envelope" {
  command = plan
  variables {
    process_cleanup_reserve_ms = 60000
    run_declaration            = { work_deadline = "2030-01-01T01:00:00Z", hard_deadline = "2030-01-01T01:00:30Z", cloud_cleanup_reserve_seconds = 1, budget_usd = 3, abort_estimate_usd = 2.5 }
  }
  expect_failures = [var.process_cleanup_reserve_ms]
}
run "offset_alias" {
  command = plan
  variables { run_declaration = { work_deadline = "2030-01-01T01:00:00+00:00", hard_deadline = "2030-01-01T02:00:00Z", cloud_cleanup_reserve_seconds = 1200, budget_usd = 3, abort_estimate_usd = 2.5 } }
  expect_failures = [var.run_declaration]
}
run "fraction_alias" {
  command = plan
  variables { run_declaration = { work_deadline = "2030-01-01T01:00:00.100Z", hard_deadline = "2030-01-01T02:00:00Z", cloud_cleanup_reserve_seconds = 1200, budget_usd = 3, abort_estimate_usd = 2.5 } }
  expect_failures = [var.run_declaration]
}

run "services_missing_role" {
  command = plan
  variables { nodes = { "smoke" = { "roles" = ["master"], "size" = "Standard_D2s_v5", "private_ip" = "10.84.1.10", "cache_disk_gib" = 0, "format_allowed" = false, "services" = [] } } }
  expect_failures = [var.nodes]
}

run "services_null" {
  command = plan
  variables { nodes = { "smoke" = { "roles" = ["master"], "size" = "Standard_D2s_v5", "private_ip" = "10.84.1.10", "cache_disk_gib" = 0, "format_allowed" = false, "services" = null } } }
  expect_failures = [var.nodes]
}

run "services_duplicate_name" {
  command = plan
  variables { nodes = { "smoke" = { "roles" = ["master"], "size" = "Standard_D2s_v5", "private_ip" = "10.84.1.10", "cache_disk_gib" = 0, "format_allowed" = false, "services" = [{ "name" = "master_http", "port" = 9444 }, { "name" = "master_http", "port" = 19444 }] } } }
  expect_failures = [var.nodes]
}

run "services_duplicate_port" {
  command = plan
  variables { nodes = { "smoke" = { "roles" = ["master"], "size" = "Standard_D2s_v5", "private_ip" = "10.84.1.10", "cache_disk_gib" = 0, "format_allowed" = false, "services" = [{ "name" = "master_http", "port" = 9444 }, { "name" = "master_grpc", "port" = 9444 }] } } }
  expect_failures = [var.nodes]
}

run "services_unknown_name" {
  command = plan
  variables { nodes = { "smoke" = { "roles" = ["master"], "size" = "Standard_D2s_v5", "private_ip" = "10.84.1.10", "cache_disk_gib" = 0, "format_allowed" = false, "services" = [{ "name" = "master_http", "port" = 9444 }, { "name" = "master_grpc", "port" = 19444 }, { "name" = "unknown", "port" = 9000 }] } } }
  expect_failures = [var.nodes]
}

run "services_out_of_range" {
  command = plan
  variables { nodes = { "smoke" = { "roles" = ["master"], "size" = "Standard_D2s_v5", "private_ip" = "10.84.1.10", "cache_disk_gib" = 0, "format_allowed" = false, "services" = [{ "name" = "master_http", "port" = 65536 }, { "name" = "master_grpc", "port" = 19444 }] } } }
  expect_failures = [var.nodes]
}

run "services_fraction_port" {
  command = plan
  variables { nodes = { "smoke" = { "roles" = ["master"], "size" = "Standard_D2s_v5", "private_ip" = "10.84.1.10", "cache_disk_gib" = 0, "format_allowed" = false, "services" = [{ "name" = "master_http", "port" = 9444.5 }, { "name" = "master_grpc", "port" = 19444 }] } } }
  expect_failures = [var.nodes]
}
run "canonical_fraction" {
  command = plan
  variables { run_declaration = { work_deadline = "2030-01-01T01:00:00.1Z", hard_deadline = "2030-01-01T02:00:00.1Z", cloud_cleanup_reserve_seconds = 1200, budget_usd = 3, abort_estimate_usd = 2.5 } }
}
