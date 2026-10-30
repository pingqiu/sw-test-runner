# Static validation root only. No credential, topology or paid-apply defaults.
provider "google" {
  project = var.project_id
  region  = var.region
  zone    = var.zone
}
module "run" {
  source                   = "../gcp"
  project_id               = var.project_id
  region                   = var.region
  zone                     = var.zone
  network_self_link        = var.network_self_link
  subnetwork_self_link     = var.subnetwork_self_link
  image                    = var.image
  run_id                   = var.run_id
  artifact_manifest_sha256 = var.artifact_manifest_sha256
  operator_cidr            = var.operator_cidr
  ssh_public_key           = var.ssh_public_key
  ssh_user                 = var.ssh_user
  credential_ref           = var.credential_ref
  nodes                    = var.nodes
  run_declaration          = var.run_declaration
}
output "inventory_json" { value = module.run.inventory_json }
output "run_declaration" { value = module.run.run_declaration }
output "managed_resource_ids" { value = module.run.managed_resource_ids }
output "resource_bindings" { value = module.run.resource_bindings }
