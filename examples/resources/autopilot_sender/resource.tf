# Make the sender's two secrets in the sender's own configuration, with a plain
# random_password (not an ephemeral value, which changes on every run). Pass
# them to AutoPilot here, and store them wherever the sender reads its secrets.
resource "random_password" "request_secret" {
  length  = 48
  special = false
}

resource "random_password" "callback_secret" {
  length  = 48
  special = false
}

resource "autopilot_sender" "tracker" {
  name = "tracker"

  # Where AutoPilot may send this sender's callbacks (https, port 443).
  callback_hosts = ["tracker.example.com"]

  # The kinds of task it may send. AutoPilot's default is implement-work-item.
  kinds = ["implement-work-item", "fix-error", "research"]

  # What its branches start with. AutoPilot's default is the name and a /.
  branch_prefix = "tracker/"

  # Write-only: sent on apply, never stored in the plan or state. To rotate
  # one, replace its random_password; the new fingerprint plans the update.
  request_secret_wo  = random_password.request_secret.result
  callback_secret_wo = random_password.callback_secret.result
}
