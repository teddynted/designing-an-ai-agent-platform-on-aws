#!/usr/bin/env bash
# Measure what a custom AMI actually saves, by launching both and timing them.
#
#   ./startup-benchmark.sh
#
# The comparison is deliberately exact. Both instances are the same type, in the
# same subnet, from the same account, launched minutes apart. The ONLY difference
# is where the software came from:
#
#   A. install-at-boot — the stock Amazon Linux 2023 image, running the very same
#      provision.sh the AMI was built with, as UserData. So the work is identical;
#      it is merely being done at the worst possible moment.
#   B. baked          — the custom AMI, where provision.sh already ran, at build
#      time, on a machine nobody was waiting for.
#
# Running the same script both ways is what makes this a measurement rather than
# an anecdote: nothing differs except *when* the work happened.
#
# The number reported is cloud-init's own: the time from boot to the end of
# UserData, which is the honest definition of "when could this instance have
# started doing something useful?". It excludes the EC2 provisioning that both
# paths pay equally.
set -euo pipefail

PROJECT="${PROJECT:-aiap}"
REGION="${REGION:-us-east-1}"
INSTANCE_TYPE="${INSTANCE_TYPE:-c5.xlarge}"
AMI_ID="${AMI_ID:-}"

say() { printf '  %s\n' "$*"; }
die() { printf '\nerror: %s\n' "$*" >&2; exit 1; }

[[ -n "$AMI_ID" ]] || die "no custom AMI to compare against — run 'make ami' first"

BUCKET="$(aws cloudformation describe-stacks --stack-name "$PROJECT-dev-04-storage" --region "$REGION" \
  --query "Stacks[0].Outputs[?OutputKey=='ArtifactBucketName'].OutputValue" --output text)"
PROFILE="$(aws cloudformation describe-stacks --stack-name "$PROJECT-dev-02-iam" --region "$REGION" \
  --query "Stacks[0].Outputs[?OutputKey=='InstanceProfileName'].OutputValue" --output text)"
SUBNET="$(aws cloudformation describe-stacks --stack-name "$PROJECT-dev-01-network" --region "$REGION" \
  --query "Stacks[0].Outputs[?OutputKey=='PublicSubnetId'].OutputValue" --output text)"
SG="$(aws cloudformation describe-stacks --stack-name "$PROJECT-dev-01-network" --region "$REGION" \
  --query "Stacks[0].Outputs[?OutputKey=='InstanceSecurityGroupId'].OutputValue" --output text)"
BASE_AMI="$(aws ssm get-parameter --region "$REGION" \
  --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 \
  --query 'Parameter.Value' --output text)"

# The AMI build inputs, so instance A can run the very same provision.sh the image
# was built from. Note the full key prefix: `aws s3 ls` prints only the leaf
# ("v1.0.0/"), and syncing from a prefix that does not exist copies nothing and
# still exits 0 — so a wrong path here does not fail, it silently produces an
# instance that installed nothing and a "boot time" that is a lie.
S3_PREFIX="ami-build/$(aws s3 ls "s3://$BUCKET/ami-build/" | awk '{print $2}' | tail -1 | tr -d /)"
[[ "$S3_PREFIX" != "ami-build/" ]] || die "no AMI build inputs in s3://$BUCKET/ami-build/ — run 'make ami' first"
aws s3 ls "s3://$BUCKET/$S3_PREFIX/provision.sh" >/dev/null \
  || die "provision.sh not found at s3://$BUCKET/$S3_PREFIX/ — the benchmark would measure nothing"

INSTANCES=()
cleanup() {
  if [[ ${#INSTANCES[@]} -gt 0 ]]; then
    printf '\nterminating the benchmark instances\n'
    aws ec2 terminate-instances --region "$REGION" --instance-ids "${INSTANCES[@]}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

launch() {
  local label="$1" ami="$2" user_data="$3"
  aws ec2 run-instances --region "$REGION" \
    --image-id "$ami" --instance-type "$INSTANCE_TYPE" \
    --subnet-id "$SUBNET" --security-group-ids "$SG" \
    --iam-instance-profile "Name=$PROFILE" \
    --metadata-options "HttpTokens=required,HttpEndpoint=enabled" \
    --user-data "$user_data" \
    --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$PROJECT-benchmark-$label},{Key=Project,Value=$PROJECT},{Key=Ephemeral,Value=true}]" \
    --query 'Instances[0].InstanceId' --output text
}

# A. Install at boot: the same provision.sh, run as UserData.
UD_TRADITIONAL="$(cat <<EOF
#!/bin/bash
set -euo pipefail
mkdir -p /tmp/ami
aws s3 sync "s3://$BUCKET/$S3_PREFIX/" /tmp/ami/ --region $REGION
chmod +x /tmp/ami/*.sh
INSTALL_OLLAMA=false bash /tmp/ami/provision.sh
EOF
)"

# B. Baked: configuration only — the same shape as the compute stack's baked path.
UD_BAKED="$(cat <<'EOF'
#!/bin/bash
set -euo pipefail
cat > /etc/spot-drain.env <<ENV
ARTIFACT_BUCKET="benchmark"
ARTIFACT_DIR="/var/lib/ai-platform/artifacts"
DRAIN_LOG="/var/log/spot-drain.log"
DRAIN_UNITS=""
POLL_SECONDS="5"
AWS_DEFAULT_REGION="us-east-1"
ENV
systemctl start spot-drain.service
EOF
)"

printf '\nStartup benchmark — %s in %s\n' "$INSTANCE_TYPE" "$REGION"
say "install-at-boot : $BASE_AMI (stock Amazon Linux 2023)"
say "baked           : $AMI_ID (custom)"

printf '\nlaunching both\n'
A="$(launch traditional "$BASE_AMI" "$UD_TRADITIONAL")"; INSTANCES+=("$A"); say "install-at-boot : $A"
B="$(launch baked "$AMI_ID" "$UD_BAKED")";              INSTANCES+=("$B"); say "baked           : $B"

aws ec2 wait instance-running --region "$REGION" --instance-ids "$A" "$B"

# Wait for both to report to SSM, then ask cloud-init how long it took. cloud-init
# is the right source: it is measuring itself, from inside, with no clock skew and
# no guessing about when the instance "really" started.
printf '\nwaiting for both to finish booting (the slow one sets the pace)\n'
for i in $(seq 1 120); do
  online="$(aws ssm describe-instance-information --region "$REGION" \
    --filters "Key=InstanceIds,Values=$A,$B" \
    --query 'length(InstanceInformationList[?PingStatus==`Online`])' --output text 2>/dev/null || echo 0)"
  [[ "$online" == "2" ]] && break
  sleep 5
done

# Guard against the failure that makes this whole exercise worthless: a boot that
# did not actually install anything looks *fast*. The first run of this benchmark
# reported install-at-boot finishing in 1.26 seconds — because a bad S3 path meant
# it downloaded nothing, `chmod` failed, and UserData exited. A very quick boot is
# not a fast boot; it is a broken one, and it would have flattered the AMI by an
# order of magnitude.
#
# So a result is only reported if the instance can prove it has the software.
validate() {
  local out="$1" label="$2"
  if grep -q "ABSENT" <<<"$out"; then
    printf '\n  !! %s did not actually install the software — this number is MEANINGLESS.\n' "$label"
    printf '     A boot that installs nothing finishes fast. Check the UserData ran.\n'
    return 1
  fi
  return 0
}

result() {
  local id="$1" label="$2"
  # `cloud-init status --wait` blocks until UserData is finished, so the timing we
  # read afterwards is of a completed boot, not one still in progress.
  local cmd
  cmd="$(aws ssm send-command --region "$REGION" --instance-ids "$id" \
    --document-name AWS-RunShellScript --timeout-seconds 900 \
    --parameters 'commands=["cloud-init status --wait > /dev/null 2>&1 || true","cloud-init analyze show 2>/dev/null | grep -E \"^Total Time\"","cloud-init analyze blame 2>/dev/null | grep -m1 config-scripts-user","systemctl is-active spot-drain.service || true","docker --version 2>/dev/null || echo \"docker: ABSENT\"","/usr/local/go/bin/go version 2>/dev/null || echo \"go: ABSENT\""]' \
    --query 'Command.CommandId' --output text)"
  for _ in $(seq 1 100); do
    status="$(aws ssm get-command-invocation --region "$REGION" --command-id "$cmd" \
      --instance-id "$id" --query 'Status' --output text 2>/dev/null || echo Pending)"
    [[ "$status" == "Success" || "$status" == "Failed" ]] && break
    sleep 10
  done
  local out
  out="$(aws ssm get-command-invocation --region "$REGION" --command-id "$cmd" \
    --instance-id "$id" --query 'StandardOutputContent' --output text)"
  printf '\n--- %s (%s) ---\n%s\n' "$label" "$id" "$out"
  validate "$out" "$label" || VALID=false
}

VALID=true
result "$A" "A · install-at-boot"
result "$B" "B · baked (custom AMI)"

printf '\nRead "Total Time" and the config-scripts-user line: the first is the whole\n'
printf 'boot, the second is UserData alone — which is the part the AMI removes.\n'

if [[ "$VALID" != true ]]; then
  printf '\nBENCHMARK INVALID — at least one instance never installed the software.\n'
  printf 'Do not quote these numbers.\n\n'
  exit 1
fi
printf '\nBoth instances verified to have the software. The comparison is valid.\n\n'
