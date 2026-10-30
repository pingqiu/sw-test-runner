variable "project_id" {
  type = string
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "Declare the existing GCP project ID."
  }
}
variable "region" { type = string }
variable "zone" { type = string }
variable "network_self_link" { type = string }
variable "subnetwork_self_link" { type = string }
variable "image" {
  description = "Exact Ubuntu 24.04 image resource, never an image family."
  type        = string
  validation {
    condition     = can(regex("^projects/[a-z0-9-]+/global/images/[a-z0-9-]+$", var.image))
    error_message = "Pin projects/PROJECT/global/images/IMAGE; image families are unsupported."
  }
}

variable "run_id" {
  type = string
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,23}$", var.run_id))
    error_message = "run_id must be a short lower-case slug."
  }
}

variable "artifact_manifest_sha256" {
  type = string
  validation {
    condition     = can(regex("^[a-f0-9]{64}$", var.artifact_manifest_sha256))
    error_message = "Supply the exact pinned artifact manifest SHA256."
  }
}

variable "operator_cidr" {
  description = "Only this controller IPv4 /32 may connect to SSH."
  type        = string
  validation {
    condition     = can(cidrnetmask(var.operator_cidr)) && endswith(var.operator_cidr, "/32")
    error_message = "SSH admission requires an IPv4 /32, never a public range."
  }
}

variable "ssh_public_key" {
  description = "Public key only. Private keys remain on the Linux controller."
  type        = string
  validation {
    condition     = can(regex("^(ssh-ed25519|ssh-rsa) [A-Za-z0-9+/=]+( .*)?$", trimspace(var.ssh_public_key))) && !strcontains(var.ssh_public_key, "PRIVATE")
    error_message = "Supply an OpenSSH public key, never a credential blob."
  }
}

variable "ssh_user" {
  type    = string
  default = "swtest"
}

variable "credential_ref" {
  description = "Logical controller-side credential reference, not a file or secret."
  type        = string
  default     = "operator-ssh"
}

variable "nodes" {
  description = "Explicit node map. Adding a node must not reindex existing IPs. Volume nodes require a cache disk."
  type = map(object({
    roles          = set(string)
    size           = string
    private_ip     = string
    cache_disk_gib = number
    format_allowed = bool
  }))
  validation {
    condition = length(var.nodes) > 0 && length(var.nodes) <= 32 && alltrue([for id, n in var.nodes :
      can(regex("^[a-z][a-z0-9-]{0,23}$", id)) && length(n.roles) > 0 &&
      length(setsubtract(n.roles, toset(["master", "filer", "volume", "s3", "client"]))) == 0 &&
      can(cidrnetmask("${n.private_ip}/32")) &&
      n.cache_disk_gib >= 0 && n.cache_disk_gib == floor(n.cache_disk_gib) &&
      (!contains(n.roles, "volume") || n.cache_disk_gib > 0) &&
      (n.cache_disk_gib > 0 || !n.format_allowed)
    ]) && length(distinct([for n in values(var.nodes) : n.private_ip])) == length(var.nodes)
    error_message = "Require 1..32 unique IPv4 nodes, supported roles, explicit integer disk sizes, and a disk for each volume."
  }
}

variable "run_declaration" {
  description = "Declared UTC deadlines and USD budget only. No authority, watchdog or provider deadline is installed by this module."
  type = object({
    work_deadline                 = string
    hard_deadline                 = string
    cloud_cleanup_reserve_seconds = number
    budget_usd                    = number
    abort_estimate_usd            = number
  })
  validation {
    condition     = can(formatdate("YYYY", var.run_declaration.work_deadline)) && endswith(var.run_declaration.work_deadline, "Z") && can(formatdate("YYYY", var.run_declaration.hard_deadline)) && endswith(var.run_declaration.hard_deadline, "Z")
    error_message = "Declare RFC3339 UTC work/hard deadlines ending Z. Data is not admission."
  }
  validation {
    condition     = var.run_declaration.cloud_cleanup_reserve_seconds > 0 && var.run_declaration.cloud_cleanup_reserve_seconds == floor(var.run_declaration.cloud_cleanup_reserve_seconds) && try(timecmp(timeadd(var.run_declaration.work_deadline, format("%ds", var.run_declaration.cloud_cleanup_reserve_seconds)), var.run_declaration.hard_deadline) <= 0, false)
    error_message = "Hard deadline must include the explicit positive integer cloud-cleanup reserve after work deadline."
  }
  validation {
    condition     = var.run_declaration.budget_usd > 0 && var.run_declaration.abort_estimate_usd > 0 && var.run_declaration.abort_estimate_usd <= var.run_declaration.budget_usd
    error_message = "Declare positive USD budget and abort estimate no greater than that budget."
  }
}
