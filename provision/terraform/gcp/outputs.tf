locals {
  owned_ids = sort(concat(
    [google_compute_firewall.ssh.self_link, google_compute_firewall.services.self_link, google_compute_firewall.deny_other.self_link],
    [for n in google_compute_instance.node : n.self_link],
    [for d in google_compute_disk.boot : d.self_link],
    [for d in google_compute_disk.cache : d.self_link],
    [for ip in google_compute_address.node : ip.self_link]
  ))
  inventory = {
    schema                   = "seaweed.inventory/v1"
    state                    = "requested"
    target                   = "gcp"
    transport                = "http"
    run_id                   = var.run_id
    artifact_manifest_sha256 = var.artifact_manifest_sha256
    managed_resource_ids     = local.owned_ids
    hosts = [for id in sort(keys(var.nodes)) : {
      id          = id
      resource_id = google_compute_instance.node[id].self_link
      roles       = sort(tolist(var.nodes[id].roles))
      management  = { address = google_compute_address.node[id].address, port = 22, user = var.ssh_user, credential_ref = var.credential_ref, host_key = "" }
      data        = { address = var.nodes[id].private_ip, nic = "", mtu = 0 }
      run_root    = "/data/seaweed-runs/${var.run_id}"
      disks       = var.nodes[id].cache_disk_gib > 0 ? [{ id = "cache", device_id = "", mount = "/mnt/seaweed-cache", format_allowed = var.nodes[id].format_allowed }] : []
      rdma        = { status = "not_applicable" }
    }]
  }
}
output "inventory_json" { value = jsonencode(local.inventory) }
output "managed_resource_ids" { value = local.owned_ids }
output "resource_bindings" {
  description = "Declared binding/generation outputs; independent live readback is still required."
  value = { for id, n in google_compute_instance.node : id => {
    instance_unique_id   = n.instance_id
    boot_disk_unique_id  = google_compute_disk.boot[id].disk_id
    cache_disk_unique_id = try(google_compute_disk.cache[id].disk_id, "")
    primary_nic_name     = n.network_interface[0].name
  } }
}
