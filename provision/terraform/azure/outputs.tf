locals {
  # OS disks are VM children, not independent azurerm_managed_disk resources.
  # Include their exact IDs in reconciliation, even after a partial apply.
  owned_ids = sort(concat(
    [azurerm_virtual_network.run.id, azurerm_subnet.run.id, azurerm_network_security_group.run.id],
    [for n in azurerm_linux_virtual_machine.node : n.id],
    [for n in azurerm_linux_virtual_machine.node : n.os_disk[0].id],
    [for n in azurerm_network_interface.node : n.id],
    [for n in azurerm_public_ip.node : n.id],
    [for d in azurerm_managed_disk.cache : d.id]
  ))
  inventory = {
    schema                   = "seaweed.inventory/v1"
    state                    = "requested"
    target                   = "azure"
    transport                = "http"
    run_id                   = var.run_id
    artifact_manifest_sha256 = var.artifact_manifest_sha256
    managed_resource_ids     = local.owned_ids
    deadlines = {
      work               = var.run_declaration.work_deadline
      hard               = var.run_declaration.hard_deadline
      cleanup_reserve_ms = var.process_cleanup_reserve_ms
    }
    hosts = [for id in sort(tolist(setintersection(toset(keys(var.nodes)), toset(keys(azurerm_linux_virtual_machine.node)), toset(keys(azurerm_public_ip.node))))) : {
      id          = id
      resource_id = azurerm_linux_virtual_machine.node[id].id
      roles       = sort(tolist(var.nodes[id].roles))
      services    = var.nodes[id].services
      management = {
        address        = azurerm_public_ip.node[id].ip_address
        port           = 22
        user           = var.ssh_user
        credential_ref = var.credential_ref
        host_key       = ""
      }
      data     = { address = var.nodes[id].private_ip, nic = "", mtu = 0 }
      run_root = "/data/seaweed-runs/${var.run_id}"
      disks = var.nodes[id].cache_disk_gib > 0 ? [{
        id             = "cache"
        device_id      = ""
        mount          = "/mnt/seaweed-cache"
        format_allowed = var.nodes[id].format_allowed
      }] : []
      rdma = { status = "not_applicable" }
    }]
  }
}
output "inventory_json" {
  description = "Requested inventory bytes only; never execution admission or observed device identity."
  value       = jsonencode(local.inventory)
}
output "managed_resource_ids" {
  value = local.owned_ids
}

output "run_declaration" {
  description = "Typed provision declaration; inventory carries the same work/hard dates. Neither output grants S0b authority."
  value = {
    schema                        = "seaweed.provision-declaration/v1"
    run_id                        = var.run_id
    artifact_manifest_sha256      = var.artifact_manifest_sha256
    work_deadline                 = var.run_declaration.work_deadline
    hard_deadline                 = var.run_declaration.hard_deadline
    cloud_cleanup_reserve_seconds = var.run_declaration.cloud_cleanup_reserve_seconds
    currency                      = "USD"
    budget_usd                    = var.run_declaration.budget_usd
    abort_estimate_usd            = var.run_declaration.abort_estimate_usd
  }
}
output "os_disk_ids" { value = [for n in azurerm_linux_virtual_machine.node : n.os_disk[0].id] }
