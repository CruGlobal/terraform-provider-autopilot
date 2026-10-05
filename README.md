# Terraform Provider AutoPilot

> **Status: AI-supported, not actively maintained.** Built for an
> internal use case at Cru. Dependabot keeps dependencies and security
> advisories current automatically (patch and minor bumps auto-merge;
> majors require manual review). Feature work and bug fixes happen on
> a best-effort basis. **Pull requests and issues are welcome**, though
> they may take time to be reviewed.

AutoPilot runs AI agents on work that senders such as issue trackers send it,
and opens pull requests a person reviews.

`terraform-provider-autopilot` manages the records that say who may send
AutoPilot work and what each app allows, through AutoPilot's admin API. Each
application keeps its own records next to the rest of its infrastructure.

## Resources

| Resource | Manages |
| --- | --- |
| `autopilot_sender` | A system that sends AutoPilot tasks: the hosts its callbacks may go to, the kinds of task it may send, the prefix its branches start with, and its two secrets (write-only). |
| `autopilot_app` | A unit of consent: the repositories it owns, the senders it accepts work from with the kinds it accepts from each, and the people who may start work for it themselves. |

AutoPilot takes a sender's task only when the app the task is billed to, and
the app that owns each repository the task touches, accept that sender for
that kind. An app may accept a sender that doesn't exist yet, so the two can
be applied in either order.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) 1.11 or
  later: `autopilot_sender` takes its secrets as write-only arguments.
- An AutoPilot with its admin API set up, and its admin token.
- [Go](https://golang.org/doc/install) 1.26 or later, only to build from
  source (the exact version is pinned in [`.tool-versions`](./.tool-versions)).

## Using the provider

```hcl
terraform {
  required_version = ">= 1.11"

  required_providers {
    autopilot = {
      source  = "CruGlobal/autopilot"
      version = "~> 0.1"
    }
  }
}

provider "autopilot" {
  endpoint = "https://autopilot.example.com"
  # token = var.autopilot_token   # or set AUTOPILOT_TOKEN
}

resource "random_password" "request_secret" {
  length  = 48
  special = false
}

resource "random_password" "callback_secret" {
  length  = 48
  special = false
}

resource "autopilot_sender" "tracker" {
  name               = "tracker"
  callback_hosts     = ["tracker.example.com"]
  kinds              = ["implement-work-item", "fix-error"]
  request_secret_wo  = random_password.request_secret.result
  callback_secret_wo = random_password.callback_secret.result
}

resource "autopilot_app" "billing" {
  name  = "billing"
  repos = ["example-org/billing-api"]
  accepts = [
    { sender = "tracker", kinds = ["implement-work-item", "fix-error"] },
  ]
}
```

### Provider attributes

| Attribute | Description |
| --- | --- |
| `endpoint` | Base URL of the AutoPilot. `/v1/admin` is appended. Falls back to `AUTOPILOT_ENDPOINT`. |
| `token` | Admin token, sent as a bearer token. Sensitive. Falls back to `AUTOPILOT_TOKEN`. |

Both are checked when the provider is configured: a missing value, an
endpoint that isn't an https URL with a host, or a token with spaces in it
fails the plan with an error that says which and where it came from. Plain
`http://` is allowed only for a loopback host (`localhost`, `127.0.0.1`,
`::1`), because the admin token travels in every request.

Full reference docs (generated from the provider schema) live in
[`docs/`](./docs/) and on the
[Terraform Registry](https://registry.terraform.io/providers/CruGlobal/autopilot/latest).

### A sender's secrets

AutoPilot never sends a sender's secrets back. It reports a fingerprint of
each one instead: the first 16 hex characters of the SHA-256 of
`autopilot-fingerprint:` followed by the secret. The secrets are write-only
arguments, so Terraform never stores them either, and the provider works out
the same fingerprints from the configuration:

- When a configured secret's fingerprint differs from the stored one, the
  provider plans an update that sends both secrets. That is how a rotation is
  planned (replace a `random_password`), and how secrets changed outside
  Terraform are put back. AutoPilot keeps the old pair for 24 hours, shown in
  `previous_secrets_until`.
- A secret that is unknown at plan time (made in the same apply) plans an
  update, and the apply sends it.
- `secrets_wo_version` sends the secrets again whatever the fingerprints say.
- An apply whose secrets are not the ones its plan was made with (say, from
  an ephemeral value that changes on every run) stops before sending them.
  Terraform itself reports it, as "Provider produced inconsistent final
  plan" naming `request_secret_fingerprint` or `callback_secret_fingerprint`:
  the secret changed between plan and apply. Use a stable value, such as a
  `random_password`.

Two cautions:

- `autopilot_sender` never stores the secrets, but the `random_password`
  resources that make them keep them in that configuration's state, and so
  does whatever the sender reads them from. Protect that state as you would
  the secrets.
- Make the secrets random: a `random_password` of 32 characters or more. A
  fingerprint is an unsalted SHA-256, quick to compute and shown in plans and
  state, so a secret someone could guess could be found from its
  fingerprint.

### How the provider talks to the API

- A record is keyed by its name. A create AutoPilot already holds with the
  same fields is answered with that record, so a create whose answer was lost
  is simply sent again; no `Idempotency-Key` is needed. When the name was
  already taken by something else:
  - with other values, the apply fails and says how to import it;
  - with exactly these values, an `autopilot_app` fails the same way, since
    an app is a unit of consent and another configuration may manage it. An
    `autopilot_sender` is taken over with a warning instead: its values
    include its secrets, and only a configuration that holds those secrets
    (in practice this one, after an earlier apply's state was lost) can match
    them.
- Every change and delete carries the state's `lock_version` as
  `If-Match`. If something else changed the record in between, AutoPilot
  answers `409 stale_object`, and the provider reports it instead of
  overwriting; run `terraform plan` again to see the change. A delete is not
  an overwrite, so a stale delete reads the current version once and deletes
  with that.
- A change is safe to retry too. AutoPilot answers a change that names the
  version just before the record's, when the record already holds exactly
  what it sends, with `200`: it is the same change, whose answer was lost.
  So a change whose answer was lost is simply sent again. (Should an older
  AutoPilot answer that repeat with `stale_object`, the provider reads the
  record, and takes the change as its own only when the record is one
  version on and holds exactly what it sent.)
- `503 unavailable`, a gateway's `502` or `504`, an attempt that timed out,
  an answer cut off on the way, and a dropped connection are retried with
  backoff, and so is `429` (honouring `Retry-After`) should AutoPilot ever
  send it. The caller's own cancellation is not retried.
- AutoPilot's `404 not_found` refusal means the record is gone (a deleted
  sender reads that way), and removes it from state. Any other `404` (a
  proxy, a page that isn't AutoPilot's, an endpoint with the wrong path) is
  an error, so a misconfigured endpoint can never make Terraform forget a
  record. A `405 method_not_allowed` means the AutoPilot is likely older than
  the provider.
- An argument left out of an `autopilot_app` is not sent on create; removing
  one later sends it as `[]`, so the app holds what the configuration says.

### Deleting a sender

Deleting a sender retires it: it reads as gone, can send no new tasks, and
its queued tasks are refused, while its running tasks run on. (To stop those
too, as for a sender whose secrets have leaked, also take it out of the
apps' `accepts`.) **A deleted sender's name and branch prefix are never
freed**, so no other system can take them over (once it owes nothing,
AutoPilot keeps a tombstone of it). Only the same sender can come back: a
create with the same name, the same two secrets and the same branch prefix
revives it, at once. So:

- `terraform apply -replace` of a sender works, even while its work is
  still running: it deletes the sender and creates it again with the same
  secrets and prefix.
- A sender's `branch_prefix` is set when it is created and never changes.
  A different prefix means a new sender with a new `name`; the plan refuses
  a new prefix under the same name, before anything is deleted.
- Renaming a sender leaves the old name, and its branch prefix, taken. A
  renamed sender needs a new prefix; the plan refuses a new name with the
  old prefix, before anything is deleted.
- `create_before_destroy` doesn't fit either resource: the name is the key.

### Importing

Both resources import by name:

```sh
terraform import autopilot_sender.tracker tracker
terraform import autopilot_app.billing billing
```

A sender's secrets can't be imported. Put them in configuration: when their
fingerprints match the stored ones, the plan after the import is empty, and
when they don't, the next apply sends them.

## Building from source

```sh
git clone https://github.com/CruGlobal/terraform-provider-autopilot
cd terraform-provider-autopilot
go build ./...
```

Pre-built, GPG-signed binaries are produced by goreleaser on every GitHub
Release and published to the public Terraform Registry.

## Developing

Common workflows are defined in [`Taskfile.yaml`](./Taskfile.yaml):

```sh
task build       # compile the provider
task install     # install the binary into $GOBIN for use with dev_overrides
task test        # run the test suite against the in-process fake API
task lint        # golangci-lint
task generate    # regenerate docs from schema (needs terraform on PATH)
task testacc     # run the test suite against a live AutoPilot
task sweep       # remove the tfacc- records a failed live run left behind
```

### Testing

The test suite drives the provider through real Terraform plans and applies.
By default it runs against an in-process fake of AutoPilot's admin API
(`internal/autopilottest`) that encodes the API's contract (the bearer token,
unknown fields refused, records keyed by name, identical creates answered
with the record, `If-Match` and `lock_version`, the refusal envelope, and the
sender and app rules), so the whole provider is testable without an
AutoPilot. The terraform CLI (1.11 or later) must be on `PATH`.

The same tests run against a live AutoPilot when `TF_ACC=1` and
`AUTOPILOT_ENDPOINT` / `AUTOPILOT_TOKEN` point at one that is not the one
real senders and apps live on. Every record they make has a random name
starting with `tfacc-` and is removed when the test ends; `task sweep`
removes any a failed run left behind. The scheduled acceptance workflow
probes the target first, and skips the run (with a "target asleep" note)
when it doesn't answer. Since a deleted sender's name is never freed, each
live run leaves its test senders behind as tombstones; their random names
never collide.

An AutoPilot used for tests may limit the repositories an app can own (the
same list limits the repositories its tasks may reach, so it should hold
only repositories kept for testing). Set `AUTOPILOT_ACC_REPOS` to a
comma-separated list of `owner/name` repositories within it, or the app
tests run without repositories.

```sh
export TF_ACC=1
export AUTOPILOT_ENDPOINT=https://autopilot.example.com
export AUTOPILOT_TOKEN=...
task testacc
```

### Testing a local build against real Terraform configs

Terraform's `dev_overrides` mechanism lets you point Terraform at a
locally-built provider binary instead of resolving the provider through the
registry.

1. Build and install:

   ```sh
   task install
   ```

   `task install` prints the exact `~/.terraformrc` snippet you need. The
   path is whatever `go env GOBIN` resolves to (or `$(go env GOPATH)/bin` if
   `GOBIN` is unset).

2. Add the printed block to `~/.terraformrc` (create the file if it doesn't
   exist):

   ```hcl
   provider_installation {
     dev_overrides {
       "CruGlobal/autopilot" = "/Users/you/go/bin"
     }

     # Leaves all other providers using the normal registry flow.
     direct {}
   }
   ```

3. In your test config, **do not run `terraform init`**: `dev_overrides` are
   mutually exclusive with the lockfile. Run `terraform plan` /
   `terraform apply` directly. Terraform prints a warning that confirms the
   override is active:

   ```
   Warning: Provider development overrides are in effect
   ```

4. Iterate: `task install` after each code change to refresh the binary,
   then run `terraform plan` again.

## License

BSD 3-Clause. See [`LICENSE`](./LICENSE).
