output "box" {
  description = "What the controller needs to reach and manage the box; no credential."
  value = {
    name                = azurerm_linux_virtual_machine.box.name
    resource_group_name = local.rg
    vm_id               = azurerm_linux_virtual_machine.box.id
    public_ip           = azurerm_public_ip.box.ip_address
    ssh_user            = var.ssh_user
    size                = var.vm_size
    priority            = azurerm_linux_virtual_machine.box.priority
    eviction_policy     = azurerm_linux_virtual_machine.box.eviction_policy
  }
}
