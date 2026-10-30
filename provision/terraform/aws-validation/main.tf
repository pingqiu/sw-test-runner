# Static validation root only. No credential, topology or paid-apply defaults.
provider "aws" {
  region              = var.region
  allowed_account_ids = [var.account_id]
}
module "run" {
  source                   = "../aws"
  account_id               = var.account_id
  partition                = var.partition
  region                   = var.region
  availability_zone        = var.availability_zone
  vpc_id                   = var.vpc_id
  subnet_id                = var.subnet_id
  ami_id                   = var.ami_id
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
