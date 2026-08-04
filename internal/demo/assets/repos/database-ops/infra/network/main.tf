# Demo Terraform configuration for the seeded database-ops project. Variables, locals, and outputs
# only, so terraform init needs no provider downloads and terraform plan runs offline with nothing
# to apply.

variable "environment" {
  type    = string
  default = "production"
}

variable "subnet_count" {
  type    = number
  default = 2
}

locals {
  subnets = [for i in range(var.subnet_count) : format("10.20.%d.0/24", i)]
}

output "environment" {
  value = var.environment
}

output "subnets" {
  value = local.subnets
}
