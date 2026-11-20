variable "subscription_id" {
  description = "Subscription UUID the box lives in; the controller pins it and scopes every Azure call to it."
  type        = string
  validation {
    condition     = can(regex("^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$", var.subscription_id))
    error_message = "subscription_id must be a UUID."
  }
}
variable "resource_group_name" {
  description = "The standing box's own RG (rg-<seat>-standing-<date>); created here when create_resource_group = true."
  type        = string
  validation {
    condition     = can(regex("^rg-[a-z0-9]+-standing-[0-9]{8}$", var.resource_group_name))
    error_message = "Use rg-<seat>-standing-<yyyymmdd>: the standing box never shares a run RG."
  }
}
variable "create_resource_group" {
  type    = bool
  default = true
}
variable "location" {
  type    = string
  default = "westus2"
}
variable "name" {
  description = "Box name; also the tag value standing_box."
  type        = string
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,23}$", var.name))
    error_message = "name must be a short lower-case slug."
  }
}
variable "vm_size" {
  type    = string
  default = "Standard_D32ads_v5"
}
variable "operator_cidr" {
  description = "Only this workstation egress IPv4 /32 may connect to SSH."
  type        = string
  validation {
    condition     = can(cidrnetmask(var.operator_cidr)) && endswith(var.operator_cidr, "/32")
    error_message = "SSH admission requires an IPv4 /32, never a public range."
  }
}
variable "ssh_public_key" {
  description = "Public key of a key pair made for this box only; the private key stays on the workstation."
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
variable "image_version" {
  description = "Exact Canonical Ubuntu 24.04 server image version; no latest alias."
  type        = string
  validation {
    condition     = can(regex("^[0-9]+[.][0-9]+[.][0-9]+$", var.image_version))
    error_message = "Pin a numeric image version."
  }
}
variable "os_disk_gib" {
  type    = number
  default = 256
}
variable "idle_minutes" {
  description = "Idle guard: no SSH session and no /run/standing/job-* marker for this long -> stop."
  type        = number
  default     = 60
  validation {
    condition     = var.idle_minutes >= 15 && var.idle_minutes <= 240
    error_message = "idle_minutes must be 15..240."
  }
}
variable "network_cidr" {
  type    = string
  default = "10.85.0.0/16"
}
variable "subnet_cidr" {
  type    = string
  default = "10.85.1.0/24"
}
