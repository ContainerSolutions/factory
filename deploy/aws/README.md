# AWS kit

How to run our dispatcher at `https://factory.container-solutions.com` on AWS, in eu-west-2
([P4](../../design/proposals/P4-dispatcher-on-aws.md)). The kit is plain OpenTofu resources in
this directory. One `tofu apply` creates a VPC with one public subnet, an EC2 t4g.micro with an
Elastic IP, the dispatcher's state bucket (`factory-state-<account>`), the instance's role and a
read-only role for the state, a CloudWatch log group, a Route 53 zone with its records, and a
status-check alarm. The instance runs `factory serve` and Caddy as two containers; Caddy gets the
certificate from Let's Encrypt. The secrets are SSM parameters you put by hand (§5): OpenTofu never
sees them. It costs about **$13 a month** at list price (P4 §What it costs).

The kit deploys our instance, so its values are written in: the domain, the region, the instance
type, the image digests and `serve`'s flags. The one variable is `aws_account_id`. Another
deployment copies the directory and edits the values.

| File | What it is |
|---|---|
| `versions.tf`, `providers.tf`, `variables.tf` | OpenTofu and the AWS provider, pinned to the one account. |
| `network.tf` | the VPC, its public subnet, internet gateway and route table. |
| `state.tf` | the dispatcher's state bucket and its settings (§10). |
| `iam.tf` | the instance role and the read-only role. |
| `instance.tf` | the instance, its security group, Elastic IP, log group and the non-secret parameters (the image digests and `serve`'s flags). |
| `user_data.sh.tftpl` | the boot script: Docker, `factory-run`, `caddy-run`, the Caddyfile and the two systemd units. |
| `dns.tf`, `monitoring.tf`, `outputs.tf` | the zone and its records, the alarm and its SNS topic, the outputs. |
| `backend.hcl.example` | the backend settings for OpenTofu's own state (§3). |
| `tests/` | `tofu test` with a mocked provider (§11). |

Commands run from `deploy/aws` unless a step says otherwise.

## 1. What you need

- **OpenTofu 1.10 or later** (CI uses 1.13). Terraform 1.10 or later works too, but the committed
  lock file is OpenTofu's: `terraform init` adds its own entries.
- **The AWS CLI v2**, with the Session Manager plugin for §9's shell.
- **An admin session on the account**, in eu-west-2:

```bash
export AWS_REGION=eu-west-2
ACCOUNT=<12-digit account ID>
```

## 2. Once: OpenTofu's state bucket

OpenTofu keeps its own state in a separate bucket, made once by hand, so the repo names no bucket.
It holds no secret. Locking uses S3's lock file (`use_lockfile`), so there is no DynamoDB table.

```bash
TFSTATE=factory-tofu-state-$ACCOUNT
aws s3api create-bucket --bucket "$TFSTATE" \
  --create-bucket-configuration LocationConstraint=eu-west-2
aws s3api put-public-access-block --bucket "$TFSTATE" --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3api put-bucket-versioning --bucket "$TFSTATE" --versioning-configuration Status=Enabled
aws s3api put-bucket-encryption --bucket "$TFSTATE" --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
```

## 3. Init

```bash
cp backend.hcl.example backend.hcl    # git-ignored; set bucket to "factory-tofu-state-<account>"
tofu init -backend-config=backend.hcl
```

## 4. Apply and delegate the name

```bash
tofu apply -var aws_account_id=$ACCOUNT
tofu output name_servers
```

Give the four name servers to whoever runs `container-solutions.com`, for an `NS` record on
`factory`. Until it resolves, Caddy retries its certificate and the dispatcher is reachable only
from the box. `dig +short NS factory.container-solutions.com` shows when it does.

The `factory` unit restarts every 5 seconds until the secrets exist (§5). That is expected.

## 5. The secrets

Each secret is a SecureString parameter under `/factory/prod/env/`, named after its environment
variable. `factory-run` reads them all when the unit starts and passes each to the container by
name. Never put a secret in `instance.tf`: its parameters are plain strings, and OpenTofu's state
would hold the value.

| Parameter (`/factory/prod/env/…`) | Value |
|---|---|
| `FACTORY_OAUTH_CLIENT_ID`, `FACTORY_OAUTH_CLIENT_SECRET` | ccf-factory's client ID and client secret. |
| `FACTORY_SESSION_KEY`, `FACTORY_CLI_TOKEN` | `openssl rand -hex 32` each. Keep the CLI token: the `factory` CLI needs it. |
| `FACTORY_SECRET_GITHUB_PUBLIC_WEBHOOK_SECRET` | the secret on ccf-factory's webhook (§8). |
| `FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY` | ccf-factory's private key (PEM). |
| `FACTORY_SECRET_CCF_FACTORY_REVIEWER_APP_PRIVATE_KEY` | ccf-factory-reviewer's private key (PEM). |
| `FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN` | `openssl rand -hex 32`, while the catalog's Pool names it. |

The `FACTORY_SECRET_*` names come from the secretRefs in `compliance-framework/factory-catalog`
(`FACTORY_SECRET_<NAME>_<KEY>`, as in [`deploy/poc/README.md`](../poc/README.md) §3). A new
secretRef in the catalog means a new parameter.

Never type a value on the command line. Read a PEM from its file, and a short value from the
terminal without echo:

```bash
aws ssm put-parameter --type SecureString --overwrite \
  --name /factory/prod/env/FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY \
  --value file:///path/to/ccf-factory.pem

read -rs v && aws ssm put-parameter --type SecureString --overwrite \
  --name /factory/prod/env/FACTORY_OAUTH_CLIENT_SECRET --value "$v"; unset v
```

Then restart the dispatcher:

```bash
aws ssm send-command --instance-ids "$(tofu output -raw instance_id)" \
  --document-name AWS-RunShellScript --parameters 'commands=["systemctl restart factory"]'
```

Rotating a secret is the same: `put-parameter --overwrite`, then the restart.

## 6. Alerts

The alarm fires when the instance fails its EC2 status checks for two minutes, and notifies the
`factory-alerts` topic. Subscribe to it, then confirm the mail AWS sends:

```bash
aws sns subscribe --topic-arn "$(tofu output -raw alerts_topic_arn)" \
  --protocol email --notification-endpoint <you@container-solutions.com>
```

## 7. Check it

- `https://factory.container-solutions.com/` serves the UI once the name resolves.
- The logs are in the CloudWatch log group `/factory/prod`, one stream per container:
  `aws logs tail /factory/prod --follow --log-stream-names factory` (or `caddy`). The dispatcher's
  are JSON.

## 8. Point GitHub and the agents at it

- ccf-factory's webhook URL: `https://factory.container-solutions.com/hooks/github-public`, with
  the secret from §5.
- ccf-factory's callback URL: `https://factory.container-solutions.com/api/v1/auth/callback`.
- Agents: enroll each host against the new URL, as in [`deploy/poc/README.md`](../poc/README.md)
  §8, with `--dispatcher https://factory.container-solutions.com` (and `--server` on
  `factory agent enroll`). They keep the default long poll.

## 9. Operating

**Upgrade or roll back the dispatcher.** Change the digest (and its version comment) in
`aws_ssm_parameter.image` in `instance.tf`, then apply and restart:

```bash
tofu apply -var aws_account_id=$ACCOUNT
aws ssm send-command --instance-ids "$(tofu output -raw instance_id)" \
  --document-name AWS-RunShellScript --parameters 'commands=["systemctl restart factory"]'
```

Roll back only to a release that reads the state: a state written by a newer release may not load
on an older one. Caddy's image is `aws_ssm_parameter.caddy_image`; restart `caddy` after changing
it. `serve`'s flags are `aws_ssm_parameter.args`; restart `factory`.

**Restarts drop webhooks.** On start the dispatcher recovers what GitHub sent since just before it
started, but a webhook sent while it was down fails and GitHub doesn't retry it. After an upgrade,
redeliver anything from that window from the App's delivery log (App settings → Advanced → Recent
Deliveries → Redeliver).

**A shell on the box.** There is no SSH: use Session Manager.

```bash
aws ssm start-session --target "$(tofu output -raw instance_id)"
```

**The state, from a laptop.** Assume `factory-state-readonly`: it may read the state and nothing
else. Your own principal needs `sts:AssumeRole` on it (an admin has it). Add a profile to
`~/.aws/config`:

```ini
[profile factory-state]
role_arn       = arn:aws:iam::<account>:role/factory-state-readonly
source_profile = <your profile>
region         = eu-west-2
```

```bash
AWS_PROFILE=factory-state AWS_REGION=eu-west-2 \
  bin/factory state tasks --state-url "$(tofu output -raw state_url)"
```

**A new Amazon Linux image.** The instance ignores new images, so an apply for something else never
replaces it. Move to the latest one on purpose:

```bash
tofu apply -var aws_account_id=$ACCOUNT -replace=aws_instance.factory
```

The new instance starts with an empty disk and loads the state from the bucket. The Elastic IP
moves to it, so the name stays. Caddy gets a new certificate. A changed `user_data.sh.tftpl`
replaces the instance the same way.

## 10. The state bucket, explained

`serve` checks none of this. If you run the dispatcher on S3 without the kit, set it up the same
way (`state.tf` and `iam.tf` have the exact settings):

| Setting | Why |
|---|---|
| Block Public Access, all four settings | The state must never be public, whatever a later policy or ACL says. |
| Object ownership: bucket owner enforced | No ACLs: the account owns every object, and IAM alone decides access. |
| Default encryption: SSE-S3 (`AES256`) | Objects are encrypted at rest, with no key to manage. |
| Bucket policy: deny `s3:*` when `aws:SecureTransport` is `false` | No request without TLS, on the bucket and its objects. It grants nothing. |
| Versioning on | The guard against an accidental delete. The store never overwrites, so a version is kept only for what was deleted. |
| Lifecycle: noncurrent versions expire after 7 days | The sweeper's deletes don't pile up. |
| Lifecycle: expired delete markers are removed | Deletes don't leave markers that listing has to step over. |
| Lifecycle: incomplete multipart uploads are aborted after a day | No hidden storage from interrupted uploads. |
| No expiration of current objects | The dispatcher deletes its own log entries, under its retention. |
| Only the dispatcher's role writes | `s3:GetObject`, `s3:PutObject` and `s3:DeleteObject` on `<bucket>/factory/*`, and `s3:ListBucket` on the bucket with `s3:prefix` = `factory/*`. |
| A read-only role | `s3:GetObject` and the same `s3:ListBucket`, for `factory state`. |
| The same region as the instance | Requests stay in-region and free of transfer charges. |

The dispatcher needs `AWS_REGION` set: `serve` refuses an `s3://` URL without it.

## 11. Testing the kit

The tests mock the AWS provider and only plan: they need no credentials and never reach AWS.

```bash
tofu init -backend=false
tofu test
```

CI runs `tofu fmt -check`, `tofu validate` and `tofu test` on every change under `deploy/aws/`.
Nothing applies the kit but a person, by hand.
