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

# Both attributes fall back to AUTOPILOT_ENDPOINT and AUTOPILOT_TOKEN, so the
# admin token never has to appear in configuration.
provider "autopilot" {
  endpoint = "https://autopilot.example.com"
}
