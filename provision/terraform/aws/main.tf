locals {
  tags          = { seaweed_run = var.run_id, managed_by = "sw-test-runner" }
  disk_nodes    = { for id, n in var.nodes : id => n if n.cache_disk_gib > 0 }
  service_ports = [3306, 7333, 8080, 8333, 8888, 9333, 18080, 18888, 19333]
  arn_prefix    = "arn:${var.partition}:ec2:${var.region}:${var.account_id}"
}
resource "aws_key_pair" "run" {
  key_name   = "${var.run_id}-ssh"
  public_key = var.ssh_public_key
  tags       = local.tags
}
resource "aws_security_group" "run" {
  name        = "${var.run_id}-nodes"
  description = "Controller SSH and owned-node HTTP services only"
  vpc_id      = var.vpc_id
  tags        = local.tags
  ingress {
    description = "controller SSH"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.operator_cidr]
  }
  dynamic "ingress" {
    for_each = toset(local.service_ports)
    content {
      from_port = ingress.value
      to_port   = ingress.value
      protocol  = "tcp"
      self      = true
    }
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
resource "aws_network_interface" "node" {
  for_each        = var.nodes
  subnet_id       = var.subnet_id
  private_ips     = [each.value.private_ip]
  security_groups = [aws_security_group.run.id]
  tags            = local.tags
}
resource "aws_instance" "node" {
  for_each          = var.nodes
  ami               = var.ami_id
  instance_type     = each.value.size
  availability_zone = var.availability_zone
  primary_network_interface {
    network_interface_id = aws_network_interface.node[each.key].id
  }
  key_name                    = aws_key_pair.run.key_name
  user_data                   = file("${path.module}/cloud-init.yaml")
  user_data_replace_on_change = true
  metadata_options {
    http_tokens = "required"
  }
  root_block_device {
    volume_type           = "gp3"
    volume_size           = 32
    encrypted             = true
    delete_on_termination = true
    tags                  = local.tags
  }
  tags       = merge(local.tags, { Name = "${var.run_id}-${each.key}" })
  depends_on = [aws_eip_association.node]
  # No IAM instance profile, private key or product deployment.
}
resource "aws_eip" "node" {
  for_each = var.nodes
  domain   = "vpc"
  tags     = local.tags
}
resource "aws_eip_association" "node" {
  for_each             = var.nodes
  allocation_id        = aws_eip.node[each.key].id
  allow_reassociation  = false
  network_interface_id = aws_network_interface.node[each.key].id
}
resource "aws_ebs_volume" "cache" {
  for_each          = local.disk_nodes
  availability_zone = var.availability_zone
  size              = each.value.cache_disk_gib
  type              = "gp3"
  encrypted         = true
  tags              = local.tags
}
resource "aws_volume_attachment" "cache" {
  for_each     = local.disk_nodes
  device_name  = "/dev/sdf"
  volume_id    = aws_ebs_volume.cache[each.key].id
  instance_id  = aws_instance.node[each.key].id
  force_detach = false
}
