output "run_declaration" {
  description = "Data only; no authentic approval, watchdog, price check or inventory deadline."
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
