# Wiring example used for fmt/validate only. No deployment values default here.
terraform {
  required_version = "= 1.17.1"
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = "= 5.12.0" }
  }
}
variable "subscription_id" { type = string }
variable "resource_group_name" { type = string }
variable "location" { type = string }
variable "ssh_public_key" { type = string }
variable "host_keys" {
  type      = object({ ed25519_private = string, ed25519_public = string })
  sensitive = true
  default   = null
}
variable "operator_cidr" { type = string }
variable "image_version" { type = string }
variable "artifact_manifest_sha256" { type = string }
variable "vm_size" { type = string }
variable "cluster" {
  type    = bool
  default = false
}
variable "run_id" {
  type    = string
  default = null
}
variable "node_host_keys" {
  type      = map(object({ ed25519_private = string, ed25519_public = string }))
  sensitive = true
  default   = {}
}
locals {
  cluster_nodes = merge({ meta = {
    roles    = ["master", "filer", "s3"], size = "Standard_D4s_v5", private_ip = "10.84.1.10", cache_disk_gib = 0, format_allowed = false
    services = [for s in [{ name = "master_http", port = 9333 }, { name = "master_grpc", port = 19333 }, { name = "filer_http", port = 8888 }, { name = "filer_grpc", port = 18888 }, { name = "s3_http", port = 8333 }] : s]
    } }, { for i in range(1, 7) : "vol${i}" => {
    roles    = ["volume"], size = var.vm_size, private_ip = "10.84.1.${20 + i}", cache_disk_gib = 4, format_allowed = false
    services = [{ name = "volume_http", port = 8080 }, { name = "volume_grpc", port = 18080 }]
    } }, { for i in range(1, 4) : "client${i}" => {
    roles = ["client"], size = var.vm_size, private_ip = "10.84.1.${30 + i}", cache_disk_gib = 0, format_allowed = false, services = []
  } })
}
variable "process_cleanup_reserve_ms" { type = number }
variable "run_declaration" {
  type = object({
    work_deadline                 = string
    hard_deadline                 = string
    cloud_cleanup_reserve_seconds = number
    budget_usd                    = number
    abort_estimate_usd            = number
  })
}
provider "azurerm" {
  features {}
  subscription_id                 = var.subscription_id
  resource_provider_registrations = "none"
}
module "run" {
  source                     = "../azure"
  resource_group_name        = var.resource_group_name
  location                   = var.location
  run_id                     = coalesce(var.run_id, var.cluster ? "ten-http-gate" : "single-http-gate")
  ssh_public_key             = var.ssh_public_key
  host_keys                  = var.host_keys
  node_host_keys             = var.node_host_keys
  operator_cidr              = var.operator_cidr
  image_version              = var.image_version
  artifact_manifest_sha256   = var.artifact_manifest_sha256
  run_declaration            = var.run_declaration
  process_cleanup_reserve_ms = var.process_cleanup_reserve_ms
  nodes = var.cluster ? local.cluster_nodes : {
    node1 = {
      roles          = ["master", "filer", "volume", "s3", "client"]
      size           = var.vm_size
      private_ip     = "10.84.1.10"
      cache_disk_gib = 4
      format_allowed = false
      services = [
        { name = "master_http", port = 9333 }, { name = "master_grpc", port = 19333 },
        { name = "filer_http", port = 8888 }, { name = "filer_grpc", port = 18888 },
        { name = "volume_http", port = 8080 }, { name = "volume_grpc", port = 18080 },
        { name = "s3_http", port = 8333 }
      ]
    }
  }
}
output "inventory_json" { value = module.run.inventory_json }

output "run_declaration" { value = module.run.run_declaration }
output "managed_resource_ids" { value = module.run.managed_resource_ids }
output "os_disk_ids" { value = module.run.os_disk_ids }
