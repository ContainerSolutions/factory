# P4: The dispatcher runs on one small EC2 instance in eu-west-2

> Status: **proposed**. Workstream: RUN (the deploy kit). Builds on
> [P3](P3-dispatcher-state-in-object-storage.md): state in an S3 bucket, any number of
> dispatchers on it. IMPROVEMENTS item 9. **No contract change and no factory code change:** the
> work is a new kit under `deploy/aws/` (a Terraform module, its user-data script and a README),
> which Gustavo runs against his account. Decisions at the end (Gustavo, 2026-11-10).

## Why

The factory ships as a product, and our own instance runs at `factory.container-solutions.com`
for compliance-framework, from the catalog in `compliance-framework/factory-catalog`
(IMPROVEMENTS item 8). Today the dispatcher runs on a workstation behind an ngrok tunnel
(`deploy/poc`). It needs a home that:

- serves the UI, `/api/v1` and `/hooks` over HTTPS at that name, so the Apps' webhooks and the
  OAuth callback point there;
- keeps its state in a bucket (P3), so losing the machine loses nothing;
- runs the timed work (debounces, lease expiry, scheduled rules, approval expiry, the
  scheduler's `Refresh`, the sweeper, the installation check, the work-item resync) without
  anyone calling it;
- holds its secrets (the Apps' keys, the session key, the OAuth secret, the CLI token, the
  pool's join token) outside the image and outside the repo;
- costs as little as it can. Item 9 asked for "inside the free tier". On AWS that can't hold
  for long, whatever we pick (§What it costs): the honest goal is the cheapest setup that is
  simple to run.

## Goal

1. `https://factory.container-solutions.com` serves the dispatcher, with a certificate that
   renews itself.
2. The state is in `s3://<bucket>/factory` in eu-west-2, private to the dispatcher's identity.
3. The timed work runs on its own, as on the workstation: no tick endpoint, no scheduler.
4. Secrets live in SSM Parameter Store (SecureString) and are read at start by the instance's
   role. They are never in the image, the repo, the Terraform state or the user data. On the instance
   they are only in Docker's root-only container config while the container runs, on an
   encrypted volume.
5. One `terraform apply` creates everything but the secrets' values and the DNS delegation,
   and an upgrade is a variable change, an apply and a restart.

Non-goals:
- High availability. One instance, recovered by EC2 when its host fails. P3 makes a second
  instance safe, but nothing here needs it.
- Scale to zero (§Options: Lambda is the path if the bill ever has to be about $1).
- Running agents in AWS. Agents stay on people's machines and enroll against the new URL.

## Options

AWS has two shapes that fit, and a few that don't.

| | **EC2 VM (chosen)** | Lambda + CloudFront |
|---|---|---|
| Runs the timed work | yes: the leader's loops run, as on a laptop | only during requests: needs `POST /tick` from EventBridge Scheduler and the loops reworked to run inside requests |
| Factory code to write | none | a stack (below) |
| Webhook to task | seconds, as today | up to a tick (EventBridge's smallest rate is 1 minute), unless the webhook handler runs the work itself |
| Agents | long-poll, the defaults | poll briefly (`poll.wait: 0s`, `interval: 30s`, P3 §Agents poll briefly) |
| TLS and the name | Caddy on the box, Let's Encrypt | CloudFront with an ACM certificate (function URLs take no custom name) |
| The UI's event stream | as today | response streaming through the Lambda Web Adapter, capped at 15 minutes |
| Cold start | none | about a second per new instance (P3 Goal 4) |
| Cost per month, list price | about $13 (t4g.micro), $9.50 (t4g.nano) | about $1.10 |

**Lambda's code**, for the record (RT's, not built): an authenticated public `POST /tick` that
takes the leader lock for the call, runs each leader loop's pending work until idle or a
deadline, then lets the lock go; the webhook handler doing the same after it stores a Delivery,
so a webhook doesn't wait for the next tick; the leader lock turned from "held and renewed every
minute" into "taken per pass", since a frozen instance can't renew it; and the HTTP server run
under the Lambda Web Adapter. About three or four PRs, plus agents switched to brief polling. It
buys about $11 a month and costs latency and a new failure mode (work that only moves when a
request comes). Not worth it now.

Ruled out:
- **ECS Fargate:** no always-free compute, about $9 a month for the smallest task, and it needs
  an ALB (about $18 a month) or a public IP per task for the name.
- **An ALB in front of the VM:** about $18 a month for what Caddy does for free.
- **Lightsail** ($5 a month with a public IPv4): its instances have no IAM role, so S3 would need
  a long-lived access key on the box, which P3 rules out ("credentials always come from the
  platform's identity").
- **No public IPv4:** GitHub sends webhooks over IPv4 only, and `api.github.com` has no IPv6, so
  the instance needs one either way. A private instance would need a NAT gateway (about $35 a
  month).
- **Spot:** a t4g.micro on spot is about $2.50 a month, but an interruption drops the webhooks
  sent while it is down (below, §Restarts). Possible later; not the default.

## Design

```
GitHub (webhooks, OAuth)       people (UI, CLI)       agents (factory-agent supervise)
            \                         |                        /
             '------ https://factory.container-solutions.com -'
                                      |
                Route 53 zone factory.container-solutions.com (A → Elastic IP)
                                      |
   EC2 t4g.micro, Amazon Linux 2023 arm64, public subnet, SG: 80 and 443 in, all out
     caddy (container, host network)  :80, :443  → 127.0.0.1:8800
     factory serve (container, host network)  public 127.0.0.1:8800, ops 127.0.0.1:9090
                                      |  instance role (IMDSv2)
               S3 bucket (state)   SSM parameters (secrets)   CloudWatch Logs
```

### The instance

- **t4g.micro** (2 vCPU burstable, 1 GiB), arm64: the image is built for `linux/arm64`. Its CPU
  credits are set to **standard**, so a busy spell slows it rather than billing surplus credits.
  t4g.nano (0.5 GiB) is the cheaper fallback if the dispatcher's memory allows it; we measure on
  micro first.
- **Amazon Linux 2023**, the AMI from SSM's public parameter
  (`/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64`), so the module
  never pins an AMI ID. Docker from `dnf`; the AWS CLI is already there. AWS moves that
  parameter to each new image every few weeks, so the instance ignores later AMI changes
  (`lifecycle { ignore_changes = [ami] }`): an apply for something else never replaces it.
  Moving to a new AMI is deliberate, `terraform apply -replace=aws_instance.factory`.
- **No SSH.** The security group opens 80 and 443 only. Access is through SSM Session Manager
  (free), and commands through SSM Run Command.
- **IMDSv2 required**, hop limit 1: both containers use the host's network, so they reach the
  metadata service as the host does, and nothing else can.
- **An 8 GiB gp3 root volume, encrypted** with EBS encryption and the AWS-managed `aws/ebs` key
  (free). Nothing on it matters but Caddy's certificates, which it re-issues if they are lost,
  and the running container's config, which holds the secrets (§Two containers).
- **Recovery:** EC2's simplified automatic recovery (on by default for t4g) moves the instance to
  new hardware on a host failure, keeping its ID, volume and Elastic IP. A replaced instance
  (a changed user-data script, or a deliberate `-replace` for a new AMI) starts with an empty
  disk and loads the state from the bucket.

### The instance role

All the instance role may do, in one place:
- `s3:GetObject`, `s3:PutObject` and `s3:DeleteObject` on `arn:aws:s3:::<bucket>/factory/*`, and
  `s3:ListBucket` on the bucket with the condition `s3:prefix` = `factory/*` (§State);
- `ssm:GetParameter` and `ssm:GetParametersByPath` under `/factory/prod/` (§Secrets);
- the `AmazonSSMManagedInstanceCore` managed policy, for Session Manager and Run Command;
- `logs:CreateLogStream` and `logs:PutLogEvents` on the one log group, for the `awslogs` driver
  (§Logs and metrics).

### Two containers, two systemd units

Both images are pinned by digest. Terraform writes them, and serve's non-secret args, to SSM
parameters that the units read at start, so an upgrade is a variable change, an apply and a
restart (§Operating).

- **`factory.service`** runs `/usr/local/bin/factory-run`, which:
  1. reads every parameter under `/factory/prod/env/` with `aws ssm get-parameters-by-path
     --with-decryption`, and exports each as an environment variable named by the parameter's
     last segment (`/factory/prod/env/FACTORY_SESSION_KEY` → `FACTORY_SESSION_KEY`). A PEM keeps
     its newlines, which `docker --env-file` can't carry;
  2. reads `/factory/prod/image` and `/factory/prod/args` (serve's catalog flags, which aren't
     secret);
  3. `exec`s `docker run --rm --network host --log-driver awslogs -e <each name> -e
     AWS_REGION=eu-west-2 <image> serve --addr 127.0.0.1:8800 --ops-addr 127.0.0.1:9090
     --public-url https://factory.container-solutions.com --state-url s3://<bucket>/factory
     --log-format json <args>`.

  The values pass from the script's environment to `docker run` by name, so they never reach a
  command line or a file the script writes. Docker keeps them in the container's config
  (`/var/lib/docker/containers/<id>/config.v2.json`, root-only, and shown by `docker inspect`)
  while the container exists; `--rm` removes it with the container, and the root volume is
  encrypted. `AWS_REGION` is required: the S3 backend refuses an `s3://` URL without it. systemd
  restarts the unit on failure.
- **`caddy.service`** runs the official `caddy:2` image with host networking and a two-line
  Caddyfile:

  ```
  factory.container-solutions.com {
      reverse_proxy 127.0.0.1:8800
  }
  ```

  Caddy gets and renews a Let's Encrypt certificate over HTTP-01 on port 80, redirects HTTP to
  HTTPS, and keeps its certificates in `/var/lib/caddy`. It forwards the UI's event stream as is.
  The ops listener stays on loopback and is not proxied, as with the tunnel.

### State: the bucket and its policy

From P3 and item 9:
- **Private:** S3 Block Public Access (all four settings), object ownership "bucket owner
  enforced", default encryption SSE-S3, and a bucket policy that denies any request without TLS
  (`aws:SecureTransport` false).
- **Only the dispatcher writes:** the instance role may `GetObject`, `PutObject` and
  `DeleteObject` on `arn:aws:s3:::<bucket>/factory/*`, and `ListBucket` on the bucket with the
  condition `s3:prefix` = `factory/*`. Nothing else in the account needs it.
- **A read-only role** (optional, a variable naming who may assume it) with `GetObject`
  and the same `ListBucket`, for `factory state --state-url s3://<bucket>/factory` from a laptop.
- **Versioning on**, as the guard against an accidental delete. The store only creates and
  deletes objects, never overwrites, so a version is kept only for what was deleted. A lifecycle
  rule expires **noncurrent** versions after 7 days, so the sweeper's deletes don't pile up. The
  same rule removes expired delete markers (`ExpiredObjectDeleteMarker: true`), so deletes don't
  leave markers that listing has to step over. There is **no** rule on current objects: P3
  deletes log entries itself, under its retention.
- **eu-west-2**, with the instance, so requests stay in-region and free of transfer charges.
- The module sets all of this; `serve` checks none of it, so the README says what it is and
  why, for anyone deploying without the module.

### Secrets

SSM Parameter Store SecureString, standard tier, encrypted with the AWS-managed key `aws/ssm`:
no charge, unlike Secrets Manager's $0.40 per secret a month. Terraform creates none of the
values (its state would hold them in plain text); the README lists the names and gives the
`aws ssm put-parameter` command for each, reading the value from a file or stdin:

- `FACTORY_OAUTH_CLIENT_ID`, `FACTORY_OAUTH_CLIENT_SECRET`, `FACTORY_SESSION_KEY`,
  `FACTORY_CLI_TOKEN`;
- one `FACTORY_SECRET_<NAME>_<KEY>` per secretRef in the catalog: today the webhook secret, the
  two App keys and the pool's join token (`deploy/poc/README.md` §3 has the list).

The instance role may `ssm:GetParametersByPath` and `ssm:GetParameter` under `/factory/prod/`;
§The instance role lists everything else it may do. Rotating one is `put-parameter
--overwrite`, then a restart.

### DNS

Gustavo's team keeps `container-solutions.com`. The module creates a Route 53 public hosted
zone for `factory.container-solutions.com` and prints its four name servers; the parent gets an
`NS` record for `factory` pointing at them. In the zone:
- an `A` record for the apex, to the Elastic IP;
- a `CAA` record allowing only `letsencrypt.org`.

The Elastic IP outlives the instance, so a replaced instance keeps the name. Caddy's first
certificate waits for the delegation: until then it retries, and the dispatcher is reachable
only from the box.

### Logs and metrics

The `awslogs` driver ships the dispatcher's and Caddy's JSON logs to a CloudWatch log group with
30 days' retention (inside CloudWatch's free 5 GB a month at our volume). `/metrics` stays on
loopback; nothing scrapes it yet. A CloudWatch alarm on the instance's status checks is free
(ten alarms) and mails Gustavo through SNS; that is all the alerting.

### Restarts and lost webhooks

A restart (an upgrade, a rotated secret, a recovered host) takes a few seconds. On start, every
dispatcher recovers the deliveries GitHub sent since 10 s before it started (`recoverSlack`,
`serve_app.go`), so the start itself loses nothing. A webhook sent while the old one was
already stopped and the new one not yet started fails, and GitHub doesn't retry it: P3's
Decision 5 keeps that a known limit. The README says to redeliver from the App's delivery log
after an upgrade if anything was sent in that window. Running the new container next to the old
one before stopping it (P3 makes two dispatchers safe) would close the gap; it isn't worth the
script while upgrades are rare.

### Agents

Unchanged. An agent installs with `--dispatcher https://factory.container-solutions.com`
(`deploy/poc/README.md` §8) and keeps the default long poll: Caddy has no read timeout on a proxied request, and `LeaseTask`'s 30 s
wait is well within the dispatcher's own.

## What it costs

eu-west-2 list prices, per month, for the setup above:

| Item | Cost |
|---|---|
| t4g.micro on demand ($0.0094 an hour) | $6.86 |
| Public IPv4, the Elastic IP ($0.005 an hour) | $3.65 |
| gp3 root volume, 8 GiB | about $0.75 |
| Route 53 hosted zone | $0.50 |
| S3: about 100,000 writes and 1,300,000 reads (P3 §What it costs, "VM"), storage well under 1 GB | about $1.10 |
| CloudWatch Logs, SSM parameters, SNS, Session Manager | $0 at our volume |
| **Total** | **about $13** (about $9.50 on t4g.nano) |

**The free tier doesn't make this $0 for long.** Since 15 July 2025 a new AWS account gets
$100 in credits (and up to $100 more for trying services) instead of the old 12 months of free
EC2:
- on the **Free plan**, nothing is billed, but the account closes after six months or when the
  credits run out, unless upgraded;
- on the **Paid plan**, the credits pay the bill until they run out or expire, whichever is
  first (the expiry date is on the account's Credits page), then it is list price;
- an account that joins an AWS Organization is on the Paid plan. Whether a member account gets
  the sign-up credits is to check on the account itself.

Lambda's setup is the only one that stays about $1.10 a month after the credits (S3 and the
hosted zone; Lambda, CloudFront and EventBridge Scheduler stay inside their always-free
allowances). §Options says why it isn't worth its code today.

## The kit: `deploy/aws/`

A Terraform root module, with the boot script as the instance's `user_data`.

- **`versions.tf`:** Terraform `>= 1.10` and the `hashicorp/aws` provider `~> 6.0`, and an
  empty `backend "s3" {}` block.
- **`variables.tf`:** `domain` (no default), `region` (default `eu-west-2`), `instance_type`
  (default `t4g.micro`), `alarm_email`, `readonly_principal_arns` (optional, for the read-only
  role), `factory_image` and `caddy_image` (digests, no defaults) and `serve_args` (the catalog
  flags). Nothing account-specific is hard-coded, so anyone can deploy the product with their own
  tfvars.
- **The resources**, one file per concern (`network.tf`, `instance.tf`, `iam.tf`, `state.tf`,
  `dns.tf`, `monitoring.tf`):
  - a VPC with one public subnet and an internet gateway (free; the account's default VPC may be
    gone);
  - the security group, the Elastic IP and the instance (the AMI from SSM's public parameter);
  - the instance role and its profile (§The instance role), and the read-only role;
  - the bucket, its public access block, ownership, encryption, versioning, lifecycle rule and
    policy;
  - the hosted zone and its `A` and `CAA` records;
  - the log group, the SNS topic and its email subscription, and the status-check alarm;
  - the non-secret SSM parameters: `/factory/prod/image`, `/factory/prod/caddy-image`,
    `/factory/prod/args`.
- **`user_data.sh.tftpl`**, rendered with `templatefile()` (the bucket, the region, the domain,
  the log group): installs Docker, writes `factory-run`, the Caddyfile and the two units, and
  enables them. It holds no secret. The instance sets `user_data_replace_on_change`, so a changed
  script replaces the instance: the state is in the bucket and the Elastic IP moves over, so
  nothing is lost but Caddy's certificate, which it gets again (Let's Encrypt allows five
  identical certificates a week).
- **`outputs.tf`:** the name servers, the state URL and the instance ID.
- **`terraform.tfvars.example`** with our values (`factory.container-solutions.com`,
  eu-west-2, the factory-catalog flags) and **`backend.hcl.example`**. The real `*.tfvars` and
  `backend.hcl` stay out of git (`.gitignore`), as do `.terraform/` and any local state.
- **`README.md`:** the order of operations (§Rollout), the secrets' parameter names, upgrade and
  rollback, reading the state from a laptop, and the bucket policy explained for anyone not
  using the module.

**Terraform's own state** lives in a separate S3 bucket, created once by hand (versioned,
private, Block Public Access on), through the S3 backend with partial configuration:
`terraform init -backend-config=backend.hcl`, so the repo names no bucket or account. Locking
uses S3's own lock file (`use_lockfile = true`, Terraform 1.10 and later), so there is no
DynamoDB table. The state holds no secret value, since Terraform never sees the secrets.

CI can't apply it. `terraform fmt -check` and `terraform validate` (after `terraform init
-backend=false`) need no credentials; the kit's PR runs them by hand and says so, rather than
adding a Terraform job to CI for one module.

## Operating

```bash
# Upgrade (or roll back) the dispatcher: set factory_image to another digest in your tfvars, then
terraform apply
aws ssm send-command --instance-ids <id> --document-name AWS-RunShellScript \
  --parameters 'commands=["systemctl restart factory"]'

# A shell on the box.
aws ssm start-session --target <id>

# The state, from a laptop, read-only.
factory state tasks --state-url s3://<bucket>/factory
```

Rolling back is the same with the previous digest. A state written by a newer release may not
load on an older one (P3's schema version): the README says to roll back only to a release that
reads it.

## Rollout

1. **This proposal** (docs).
2. **`feat(deploy): the dispatcher on one EC2 instance (deploy/aws)`**: the Terraform module,
   the user-data script and the README. One PR, RUN's. It is under 1000 lines.
3. **Gustavo's steps**, in this order, from the README:
   1. create the Terraform state bucket once, copy the two examples, then `terraform init
      -backend-config=backend.hcl` and `terraform apply` in eu-west-2;
   2. give the four name servers to whoever runs `container-solutions.com`, for the `factory`
      `NS` record;
   3. `put-parameter` each secret. The image digests and serve's args come from the tfvars (the
      catalog flags: `--catalog-repo https://github.com/compliance-framework/factory-catalog
      --catalog-app-id 5343156 --catalog-app-key-ref ccf-factory-app/private-key --retention
      2160h`);
   4. restart `factory`; check `https://factory.container-solutions.com/` serves the UI once
      the delegation resolves;
   5. point ccf-factory's and ccf-factory-reviewer's webhook URL at
      `https://factory.container-solutions.com/hooks/<provider>`, and the OAuth App's callback at
      `/api/v1/auth/callback`;
   6. enroll an agent against the new URL and dispatch a test event;
   7. stop the workstation's dispatcher. Its state isn't moved (P3 Decision 2: a fresh start).

The canary on docs (item 8's cutover) follows once this is up.

**Done when** (item 9): the UI is served over HTTPS at `factory.container-solutions.com`; the
state is in the bucket; the Apps' webhooks reach it; an agent enrolls and runs a task; and the
monthly bill is the table above (about $13), paid by the credits while they last.

## Decisions (Gustavo, 2026-11-10)

1. **AWS**, in **eu-west-2**. The account is chosen later; this design doesn't depend on it.
2. **DNS:** `container-solutions.com` stays where it is, with an `NS` record delegating
   `factory.container-solutions.com` to a Route 53 hosted zone the module creates.
3. **A small always-on VM is fine.** So: one EC2 instance, the leader's loops running as on a
   laptop, no tick endpoint and no factory code change.
4. **The bill:** about $13 a month at list price is fine, so not the Lambda stack. It replaces
   item 9's "inside the free tier".
5. **Terraform**, not CloudFormation, with the install script as the instance's `user_data`.

Open, for Gustavo:
- **The account:** new and standalone (credits; the Free plan closes after six months unless
  upgraded), or a member of Container Solutions' AWS Organization (Paid plan; credits to check)?
