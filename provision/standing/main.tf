# Real provider configuration (features is required): pinned subscription, no implicit provider registration.
provider "azurerm" {
  features {}
  subscription_id                 = var.subscription_id
  resource_provider_registrations = "none"
}
locals {
  rg   = var.create_resource_group ? azurerm_resource_group.box[0].name : var.resource_group_name
  tags = { standing_box = var.name, managed_by = "sw-test-runner", purpose = "standing-test-box" }
}
resource "azurerm_resource_group" "box" {
  count    = var.create_resource_group ? 1 : 0
  name     = var.resource_group_name
  location = var.location
  tags     = local.tags
}
resource "azurerm_virtual_network" "box" {
  name                = "${var.name}-vnet"
  resource_group_name = local.rg
  location            = var.location
  address_space       = [var.network_cidr]
  tags                = local.tags
}
resource "azurerm_subnet" "box" {
  name                 = "box"
  resource_group_name  = local.rg
  virtual_network_name = azurerm_virtual_network.box.name
  address_prefixes     = [var.subnet_cidr]
}
resource "azurerm_network_security_group" "box" {
  name                = "${var.name}-nsg"
  resource_group_name = local.rg
  location            = var.location
  tags                = local.tags
  security_rule {
    name                       = "operator-ssh"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefix      = var.operator_cidr
    destination_address_prefix = var.subnet_cidr
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
resource "azurerm_subnet_network_security_group_association" "box" {
  subnet_id                 = azurerm_subnet.box.id
  network_security_group_id = azurerm_network_security_group.box.id
}
resource "azurerm_public_ip" "box" {
  name                = "${var.name}-ip"
  resource_group_name = local.rg
  location            = var.location
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = local.tags
}
resource "azurerm_network_interface" "box" {
  name                = "${var.name}-nic"
  resource_group_name = local.rg
  location            = var.location
  tags                = local.tags
  ip_configuration {
    name                          = "primary"
    subnet_id                     = azurerm_subnet.box.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.box.id
  }
}
# Spot, evicted to Deallocated (disk kept), max price -1: evicted for capacity only, never for price.
# No identity block: the box holds no Azure credential.
resource "azurerm_linux_virtual_machine" "box" {
  name                            = var.name
  resource_group_name             = local.rg
  location                        = var.location
  size                            = var.vm_size
  priority                        = "Spot"
  eviction_policy                 = "Deallocate"
  max_bid_price                   = -1
  admin_username                  = var.ssh_user
  disable_password_authentication = true
  network_interface_ids           = [azurerm_network_interface.box.id]
  tags                            = local.tags
  admin_ssh_key {
    username   = var.ssh_user
    public_key = var.ssh_public_key
  }
  os_disk {
    name                 = "${var.name}-os"
    caching              = "ReadWrite"
    storage_account_type = "Premium_LRS"
    disk_size_gb         = var.os_disk_gib
  }
  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = var.image_version
  }
  custom_data = base64encode(templatefile("${path.module}/cloud-init.yaml", { idle_minutes = var.idle_minutes }))
  depends_on  = [azurerm_subnet_network_security_group_association.box]
}
