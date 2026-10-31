variable "resource_group_name" {
  description = "Existing, dedicated test RG; this module never creates or deletes it."
  type        = string
}
variable "location" {
  type = string
}
variable "run_id" {
  type = string
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,31}$", var.run_id))
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
variable "network_cidr" {
  type    = string
  default = "10.84.0.0/16"
}
variable "subnet_cidr" {
  type    = string
  default = "10.84.1.0/24"
}
variable "image_version" {
  description = "Exact Canonical Ubuntu 24.04 server image version; no latest alias."
  type        = string
  validation {
    condition     = can(regex("^[0-9]+[.][0-9]+[.][0-9]+$", var.image_version))
    error_message = "Pin a numeric image version. Availability is a later live gate."
  }
}
variable "host_keys" {
  description = "Optional pre-generated VM server key. Private cloud-init/state only; never the controller client key."
  type        = object({ ed25519_private = string, ed25519_public = string })
  sensitive   = true
  default     = null
}
variable "nodes" {
  description = "Explicit node map. Adding a node must not reindex existing IPs. Volume nodes require a cache disk."
  type = map(object({
    roles          = set(string)
    size           = string
    private_ip     = string
    cache_disk_gib = number
    format_allowed = bool
    services       = list(object({ name = string, port = number }))
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
  validation {
    condition = alltrue([for n in values(var.nodes) : try(
      length(distinct([for s in n.services : s.name])) == length(n.services) &&
      length(distinct([for s in n.services : s.port])) == length(n.services) &&
      alltrue([for s in n.services : contains(["master_http", "master_grpc", "filer_http", "filer_grpc", "volume_http", "volume_grpc", "s3_http"], s.name) && s.port >= 1 && s.port <= 65535 && s.port != 22 && s.port == floor(s.port)]) &&
      alltrue(flatten([for role in n.roles : [for name in lookup({ master = ["master_http", "master_grpc"], filer = ["filer_http", "filer_grpc"], volume = ["volume_http", "volume_grpc"], s3 = ["s3_http"], client = [] }, role, []) : contains([for s in n.services : s.name], name)]])), false)
    ])
    error_message = "Require explicit HTTP module service names, distinct integer ports 1..65535 excluding management port 22, and every declared role's HTTP/gRPC listeners; clients may declare []."
  }
}

variable "process_cleanup_reserve_ms" {
  description = "Original S0b process reserve; distinct from cloud resource cleanup seconds. No authority is granted."
  type        = number
  validation {
    condition     = var.process_cleanup_reserve_ms > 0 && var.process_cleanup_reserve_ms <= 60000 && var.process_cleanup_reserve_ms == floor(var.process_cleanup_reserve_ms) && try(timecmp(timeadd(var.run_declaration.work_deadline, format("%dms", var.process_cleanup_reserve_ms)), var.run_declaration.hard_deadline) <= 0, false)
    error_message = "Process cleanup reserve must be an integer 1..60000 ms within the original work/hard envelope."
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
    condition     = alltrue([for t in [var.run_declaration.work_deadline, var.run_declaration.hard_deadline] : can(formatdate("YYYY", t)) && can(regex("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{0,8}[1-9])?Z$", t))])
    error_message = "Declare canonical RFC3339 UTC Z deadlines (at most 9 fractional digits; no offset alias or trailing fractional zero). Data is not admission."
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
