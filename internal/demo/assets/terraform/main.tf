# Demo Terraform configuration for Yardmaster. It declares only variables, locals, and outputs, so
# terraform init needs no provider downloads and terraform plan runs offline with nothing to apply.
# That keeps the seeded Terraform job fast and self-contained on any host that has the terraform
# binary.

variable "environment" {
  type    = string
  default = "production"
}

variable "web_count" {
  type    = number
  default = 3
}

locals {
  web_hosts = [for i in range(var.web_count) : format("web%02d", i + 1)]
}

output "environment" {
  value = var.environment
}

output "web_hosts" {
  value = local.web_hosts
}
