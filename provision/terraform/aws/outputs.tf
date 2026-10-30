locals {
  owned_ids = sort(concat(
    [aws_key_pair.run.arn, aws_security_group.run.arn],
    [for n in aws_instance.node : n.arn],
    [for n in aws_instance.node : "${local.arn_prefix}:volume/${n.root_block_device[0].volume_id}"],
    [for n in aws_network_interface.node : n.arn],
    [for ip in aws_eip.node : "${local.arn_prefix}:elastic-ip/${ip.id}"],
    [for d in aws_ebs_volume.cache : d.arn]
  ))
  inventory = {
    schema                   = "seaweed.inventory/v1"
    state                    = "requested"
    target                   = "aws"
    transport                = "http"
    run_id                   = var.run_id
    artifact_manifest_sha256 = var.artifact_manifest_sha256
    managed_resource_ids     = local.owned_ids
    hosts = [for id in sort(keys(var.nodes)) : {
      id          = id
      resource_id = aws_instance.node[id].arn
      roles       = sort(tolist(var.nodes[id].roles))
      management  = { address = aws_eip.node[id].public_ip, port = 22, user = var.ssh_user, credential_ref = var.credential_ref, host_key = "" }
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
  description = "Data for later identity-safe reconciliation; not live qualification."
  value = { for id, n in aws_instance.node : id => {
    instance_id        = n.id
    primary_eni_id     = n.primary_network_interface_id
    root_volume_id     = n.root_block_device[0].volume_id
    eip_allocation_id  = aws_eip.node[id].id
    eip_association_id = aws_eip_association.node[id].id
    cache_volume_id    = try(aws_ebs_volume.cache[id].id, "")
  } }
}
