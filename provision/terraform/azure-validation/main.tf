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
variable "operator_cidr" { type = string }
variable "image_version" { type = string }
variable "artifact_manifest_sha256" { type = string }
variable "vm_size" { type = string }
provider "azurerm" {
  features {}
  subscription_id                 = var.subscription_id
  resource_provider_registrations = "none"
}
module "run" {
  source                   = "../azure"
  resource_group_name      = var.resource_group_name
  location                 = var.location
  run_id                   = "single-http-gate"
  ssh_public_key           = var.ssh_public_key
  operator_cidr            = var.operator_cidr
  image_version            = var.image_version
  artifact_manifest_sha256 = var.artifact_manifest_sha256
  nodes = {
    node1 = {
      roles          = ["master", "filer", "volume", "s3", "client"]
      size           = var.vm_size
      private_ip     = "10.84.1.10"
      cache_disk_gib = 4
      format_allowed = false
    }
  }
}
output "inventory_json" { value = module.run.inventory_json }
