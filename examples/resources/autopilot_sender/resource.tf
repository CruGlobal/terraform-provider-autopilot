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

  # Let this sender's urgent tasks start before every queued task that isn't
  # urgent while AutoPilot is running as many tasks as it allows, and without
  # waiting for the day's spend under its daily cap on Bedrock spend (a task
  # whose own max_usd is over the whole cap still can't run on Bedrock).
  # Grant it only to a sender whose urgent tasks are few and can't wait, such
  # as fixes for a production incident. Off unless set.
  may_send_urgent = true

  # What its branches start with. AutoPilot's default is the name and a /.
  branch_prefix = "tracker/"

  # Write-only: sent on apply, never stored in the plan or state. To rotate
  # one, replace its random_password; the new fingerprint plans the update.
  request_secret_wo  = random_password.request_secret.result
  callback_secret_wo = random_password.callback_secret.result
}
