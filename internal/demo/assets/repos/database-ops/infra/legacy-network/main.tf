# The legacy network after its decommission was merged. Its three subnets are gone from this
# configuration and still in the state beside it, so a plan here destroys all three. That is the
# change the demo's terraform rule holds for a second person.
#
# The subnets are terraform_data, which Terraform and OpenTofu build in, so init needs no provider
# downloads and the plan runs offline, the same as the network root beside this one.

terraform {
  required_version = ">= 1.5.0"
}
