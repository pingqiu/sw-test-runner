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
