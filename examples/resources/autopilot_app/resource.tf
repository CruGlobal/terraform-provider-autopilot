locals {
  developers = ["first.developer@example.com", "second.developer@example.com"]
}

resource "autopilot_app" "billing" {
  name = "billing"

  # The repositories this app owns. A repository belongs to one app at most.
  repos = ["example-org/billing-api", "example-org/billing-web"]

  # The senders it accepts work from, and the kinds it accepts from each.
  accepts = [
    {
      sender = "tracker"
      kinds  = ["implement-work-item", "fix-error", "review-pr"]
      # Let the tracker's review-pr tasks approve or request changes.
      can_decide = true
    },
    {
      sender = "monitor"
      kinds  = ["fix-error"]
    },
  ]

  # The people who may start work for this app themselves.
  developers = local.developers
}
