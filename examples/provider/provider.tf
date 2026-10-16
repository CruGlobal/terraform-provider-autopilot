terraform {
  # autopilot_sender takes its secrets as write-only arguments, which need
  # Terraform 1.11 or later.
  required_version = ">= 1.11"

  required_providers {
    autopilot = {
      source  = "CruGlobal/autopilot"
      version = "~> 0.1"
    }
  }
}

# The provider takes the endpoint and the admin token from one place. Here
# both come from the AUTOPILOT_ENDPOINT and AUTOPILOT_TOKEN environment
# variables, so the token never has to appear in configuration.
provider "autopilot" {}

# Or set both in the block, and the environment is not read:
#
#   provider "autopilot" {
#     endpoint = "https://autopilot.example.com"
#     token    = var.autopilot_token
#   }
#
# A block that sets one of the two but not the other is refused, even when
# the environment has the other, so a token meant for one AutoPilot is never
# sent to another.
