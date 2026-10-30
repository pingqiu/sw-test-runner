locals {
  labels        = { seaweed_run = var.run_id, managed_by = "sw-test-runner" }
  disk_nodes    = { for id, n in var.nodes : id => n if n.cache_disk_gib > 0 }
  service_ports = ["3306", "7333", "8080", "8333", "8888", "9333", "18080", "18888", "19333"]
}
resource "google_compute_firewall" "ssh" {
  project       = var.project_id
  name          = "${var.run_id}-ssh"
  network       = var.network_self_link
  direction     = "INGRESS"
  priority      = 100
  source_ranges = [var.operator_cidr]
  target_tags   = [var.run_id]
  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}
resource "google_compute_firewall" "services" {
  project       = var.project_id
  name          = "${var.run_id}-services"
  network       = var.network_self_link
  direction     = "INGRESS"
  priority      = 110
  source_ranges = [for n in var.nodes : "${n.private_ip}/32"]
  target_tags   = [var.run_id]
  allow {
    protocol = "tcp"
    ports    = local.service_ports
  }
}
resource "google_compute_firewall" "deny_other" {
  project       = var.project_id
  name          = "${var.run_id}-deny-other"
  network       = var.network_self_link
  direction     = "INGRESS"
  priority      = 200
  source_ranges = ["0.0.0.0/0"]
  target_tags   = [var.run_id]
  deny { protocol = "all" }
}
resource "google_compute_address" "node" {
  for_each     = var.nodes
  project      = var.project_id
  region       = var.region
  name         = "${var.run_id}-${each.key}-ip"
  address_type = "EXTERNAL"
  network_tier = "PREMIUM"
}
resource "google_compute_disk" "boot" {
  for_each = var.nodes
  project  = var.project_id
  zone     = var.zone
  name     = "${var.run_id}-${each.key}-boot"
  type     = "pd-balanced"
  size     = 32
  image    = var.image
  labels   = local.labels
}
resource "google_compute_disk" "cache" {
  for_each = local.disk_nodes
  project  = var.project_id
  zone     = var.zone
  name     = "${var.run_id}-${each.key}-cache"
  type     = "pd-balanced"
  size     = each.value.cache_disk_gib
  labels   = local.labels
}
resource "google_compute_instance" "node" {
  for_each     = var.nodes
  project      = var.project_id
  zone         = var.zone
  name         = "${var.run_id}-${each.key}"
  machine_type = each.value.size
  tags         = [var.run_id]
  labels       = local.labels
  boot_disk {
    source      = google_compute_disk.boot[each.key].self_link
    auto_delete = false
  }
  dynamic "attached_disk" {
    for_each = each.value.cache_disk_gib > 0 ? [each.key] : []
    content {
      source      = google_compute_disk.cache[attached_disk.value].self_link
      device_name = "cache"
      mode        = "READ_WRITE"
    }
  }
  network_interface {
    subnetwork = var.subnetwork_self_link
    network_ip = each.value.private_ip
    access_config {
      nat_ip       = google_compute_address.node[each.key].address
      network_tier = "PREMIUM"
    }
  }
  metadata = {
    "block-project-ssh-keys" = "true"
    "enable-oslogin"         = "FALSE"
    "ssh-keys"               = "${var.ssh_user}:${var.ssh_public_key}"
    "user-data"              = file("${path.module}/cloud-init.yaml")
  }
  # No service_account block: no guest cloud identity is requested.
  depends_on = [google_compute_firewall.ssh, google_compute_firewall.services, google_compute_firewall.deny_other]
}
