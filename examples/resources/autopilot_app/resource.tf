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
      # Let GitHub's auto-merge be turned on for the pull requests the
      # tracker's tasks open, when a task asks for it. GitHub merges each one
      # once the repository allows auto-merge and its required checks and
      # reviews pass.
      can_auto_merge = true
    },
    {
      sender = "monitor"
      kinds  = ["fix-error"]
    },
    {
      # Pull request reviews the developers start from GitHub.
      sender = "autopilot-github"
      kinds  = ["review-pr"]
      # Let them approve or request changes. On a pull request that may be
      # the asker's own they still only comment, unless can_decide_own_pr
      # is set to true as well. That check goes by the pull request's author
      # alone, so read can_decide's caveats before turning this on.
      can_decide = true
      # Approve only pull requests rated low risk. An approval of one rated
      # medium or high, or one it couldn't rate, is posted as a comment
      # instead, saying why. Requests for changes aren't limited.
      approve_up_to = "low"
    },
  ]

  # The people who may start work for this app themselves.
  developers = local.developers

  # Each developer's GitHub login, so a review they start from GitHub runs
  # in their name. Every developer here must be in developers too.
  github_logins = [
    { login = "example-first-dev", developer = "first.developer@example.com" },
    { login = "example-second-dev", developer = "second.developer@example.com" },
  ]

  # What its tasks' model calls run on: a Claude seat when one has room,
  # and Bedrock when every seat is full.
  credential      = "seat_first"
  when_seats_full = "fall_back"

  # The worker its tasks run in: the ruby image, on a bigger task than the
  # default of 2 vCPU and 8 GB.
  worker_image  = "ruby"
  worker_cpu    = 4096
  worker_memory = 16384

  # Review each pull request into the default branch when it is opened or
  # marked ready, with no label, as the app's own service identity. Drafts
  # wait until they are ready, and bots' pull requests are skipped.
  auto_review = {
    events      = ["opened", "ready_for_review"]
    max_per_day = 50
    # Let them approve pull requests rated low risk. A developer's own pull
    # request still only gets a comment, unless can_decide_own_pr is set to
    # true as well. With it off, they decide only once every developer's
    # GitHub login is in github_logins: until then, any pull request may be
    # a developer's own.
    can_decide    = true
    approve_up_to = "low"
  }
}
