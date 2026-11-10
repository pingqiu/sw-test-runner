locals {
  tags          = { seaweed_run = var.run_id, managed_by = "sw-test-runner", expires_at = var.run_declaration.hard_deadline }
  disk_nodes    = { for id, n in var.nodes : id => n if n.cache_disk_gib > 0 }
  service_ports = sort(distinct(flatten([for n in values(var.nodes) : [for s in n.services : tostring(s.port)]])))
}
resource "azurerm_virtual_network" "run" {
  name                = "${var.run_id}-vnet"
  resource_group_name = var.resource_group_name
  location            = var.location
  address_space       = [var.network_cidr]
  tags                = local.tags
}
resource "azurerm_subnet" "run" {
  name                 = "nodes"
  resource_group_name  = var.resource_group_name
  virtual_network_name = azurerm_virtual_network.run.name
  address_prefixes     = [var.subnet_cidr]
}
resource "azurerm_network_security_group" "run" {
  name                = "${var.run_id}-nsg"
  resource_group_name = var.resource_group_name
  location            = var.location
  tags                = local.tags
  security_rule {
    name                       = "controller-ssh"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefix      = var.operator_cidr
    destination_address_prefix = var.subnet_cidr
  }
  dynamic "security_rule" {
    for_each = length(local.service_ports) == 0 ? [] : [1]
    content {
      name                       = "internal-services"
      priority                   = 110
      direction                  = "Inbound"
      access                     = "Allow"
      protocol                   = "Tcp"
      source_port_range          = "*"
      destination_port_ranges    = local.service_ports
      source_address_prefix      = var.subnet_cidr
      destination_address_prefix = var.subnet_cidr
    }
  }
  security_rule {
    name                       = "deny-other-inbound"
    priority                   = 200
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }
}
resource "azurerm_subnet_network_security_group_association" "run" {
  subnet_id                 = azurerm_subnet.run.id
  network_security_group_id = azurerm_network_security_group.run.id
}
resource "azurerm_public_ip" "node" {
  for_each            = var.nodes
  name                = "${var.run_id}-${each.key}-ip"
  resource_group_name = var.resource_group_name
  location            = var.location
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = local.tags
}
resource "azurerm_network_interface" "node" {
  for_each            = var.nodes
  name                = "${var.run_id}-${each.key}-nic"
  resource_group_name = var.resource_group_name
  location            = var.location
  tags                = local.tags
  ip_configuration {
    name                          = "data"
    subnet_id                     = azurerm_subnet.run.id
    private_ip_address_allocation = "Static"
    private_ip_address            = each.value.private_ip
    public_ip_address_id          = azurerm_public_ip.node[each.key].id
  }
}
locals {
  cloud_init_file = file("${path.module}/cloud-init.yaml")
  cloud_init_base = yamldecode(local.cloud_init_file)
  # A mount soak client: weed mount needs FUSE, and allow_other for the worker's mounts.
  cloud_init_fuse = {
    packages = concat(local.cloud_init_base.packages, ["fuse3"])
    runcmd   = [["sh", "-c", "grep -qx user_allow_other /etc/fuse.conf || echo user_allow_other >> /etc/fuse.conf"]]
  }
}
resource "azurerm_linux_virtual_machine" "node" {
  for_each                        = var.nodes
  name                            = "${var.run_id}-${each.key}"
  resource_group_name             = var.resource_group_name
  location                        = var.location
  size                            = each.value.size
  admin_username                  = var.ssh_user
  disable_password_authentication = true
  network_interface_ids           = [azurerm_network_interface.node[each.key].id]
  tags                            = local.tags
  admin_ssh_key {
    username   = var.ssh_user
    public_key = var.ssh_public_key
  }
  os_disk {
    name                 = "${var.run_id}-${each.key}-os"
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
    disk_size_gb         = 32
  }
  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = var.image_version
  }
  # A mount soak client (fuse_clients) also gets fuse3 and user_allow_other; every other node's cloud-init is the
  # same bytes as before: the file as is, or the file merged with its server key.
  custom_data = base64encode(try(var.node_host_keys[each.key], var.host_keys) == null && !(var.fuse_clients && contains(each.value.roles, "client")) ? local.cloud_init_file : "#cloud-config\n${yamlencode(merge(concat(
    [local.cloud_init_base],
    [for _ in(var.fuse_clients && contains(each.value.roles, "client") ? [1] : []) : local.cloud_init_fuse],
    [for k in(try(var.node_host_keys[each.key], var.host_keys) == null ? [] : [try(var.node_host_keys[each.key], var.host_keys)]) : { ssh_keys = k }],
  )...))}")
  depends_on = [azurerm_subnet_network_security_group_association.run]
}
resource "azurerm_managed_disk" "cache" {
  for_each             = local.disk_nodes
  name                 = "${var.run_id}-${each.key}-cache"
  resource_group_name  = var.resource_group_name
  location             = var.location
  storage_account_type = "StandardSSD_LRS"
  create_option        = "Empty"
  disk_size_gb         = each.value.cache_disk_gib
  tags                 = local.tags
}
resource "azurerm_virtual_machine_data_disk_attachment" "cache" {
  for_each           = local.disk_nodes
  managed_disk_id    = azurerm_managed_disk.cache[each.key].id
  virtual_machine_id = azurerm_linux_virtual_machine.node[each.key].id
  lun                = 0
  caching            = "None"
}
