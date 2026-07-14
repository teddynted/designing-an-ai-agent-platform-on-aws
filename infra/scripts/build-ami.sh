#!/usr/bin/env bash
# Build a versioned custom AMI for the AI Agent Platform.
#
#   ./build-ami.sh                       # next patch version
#   AMI_VERSION=2.0.0 ./build-ami.sh     # an explicit version
#   INSTALL_OLLAMA=true ./build-ami.sh   # include Ollama (~1.5 GB larger)
#
# WHY THIS IS A SCRIPT AND NOT CLOUDFORMATION
#
# CloudFormation has no resource type that builds an AMI. It can *consume* one
# (that is what the AmiId parameter on the compute stack is for), but producing
# one is a procedure — launch, provision, verify, snapshot — not a declaration.
# Pretending otherwise means a custom resource wrapping this same procedure in a
# Lambda, which is more moving parts for the same steps. Building the image is a
# pipeline concern; consuming it is an infrastructure concern. This script is the
# former, and the compute stack is the latter, and the AMI ID is the interface
# between them.
#
# EC2 Image Builder is the managed version of exactly this, and for a large fleet
# it is the right answer. It is deliberately not used here: this milestone's
# service list is EC2, AMIs, CloudFormation, IAM, and CloudWatch Logs, and the
# mechanics are the thing worth learning — they are the same mechanics Image
# Builder runs on your behalf.
#
# THE BUILD IS TWO PHASES
#
#   1. provision (UserData) — install everything, write a manifest, plant a marker.
#   2. cleanup (over SSM)   — strip credentials and cloud-init state, shut down.
#
# They are separate because cleanup destroys what provision stands on: it deletes
# cloud-init's copy of the running script, and stops the SSM agent that reports
# the result. So phase 1 must be *confirmed complete* before phase 2 begins.
set -euo pipefail

PROJECT="${PROJECT:-aiap}"
REGION="${REGION:-us-east-1}"
# The builder is On-Demand on purpose. A Spot builder can be reclaimed halfway
# through a ten-minute build, and a half-built image is the one failure mode an
# AMI pipeline must never produce. It runs for minutes and costs cents.
BUILDER_TYPE="${BUILDER_TYPE:-t3.large}"
INSTALL_OLLAMA="${INSTALL_OLLAMA:-false}"
GO_VERSION="${GO_VERSION:-1.23.4}"
NODE_MAJOR="${NODE_MAJOR:-20}"
COMPOSE_VERSION="${COMPOSE_VERSION:-v2.32.1}"
# How long to wait for the provision phase before giving up. A first build pulls
# a few hundred MB; ten minutes is generous, and hanging forever is not a plan.
BUILD_TIMEOUT_SECONDS="${BUILD_TIMEOUT_SECONDS:-1200}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AMI_DIR="$SCRIPT_DIR/ami"
NAME_PREFIX="$PROJECT-platform"

say()  { printf '  %s\n' "$*"; }
die()  { printf '\nerror: %s\n' "$*" >&2; exit 1; }

command -v aws >/dev/null || die "the AWS CLI is required"
command -v jq  >/dev/null || die "jq is required"

# ---------------------------------------------------------------------------
# Version. Semantic, and the AMI's name is derived from it, so the name alone
# tells you what you are looking at: aiap-platform-v1.2.0.
#
# Nothing is ever overwritten: an AMI name is unique per account and region, so a
# rebuild of an existing version is a hard error rather than a silent replacement
# of an image that instances may still be running.
# ---------------------------------------------------------------------------
latest_version() {
  aws ec2 describe-images --owners self --region "$REGION" \
    --filters "Name=tag:Project,Values=$PROJECT" "Name=tag:Component,Values=platform-ami" \
    --query 'Images[].Tags[?Key==`Version`].Value' --output text 2>/dev/null \
    | tr '\t' '\n' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -t. -k1,1n -k2,2n -k3,3n | tail -1
}

if [[ -z "${AMI_VERSION:-}" ]]; then
  previous="$(latest_version || true)"
  if [[ -z "$previous" ]]; then
    AMI_VERSION="1.0.0"
  else
    IFS=. read -r major minor patch <<<"$previous"
    AMI_VERSION="$major.$minor.$((patch + 1))"
  fi
fi

AMI_NAME="$NAME_PREFIX-v$AMI_VERSION"

printf '\nBuilding %s\n' "$AMI_NAME"
say "region       : $REGION"
say "builder      : $BUILDER_TYPE (on-demand)"
say "ollama       : $INSTALL_OLLAMA"

if aws ec2 describe-images --owners self --region "$REGION" \
     --filters "Name=name,Values=$AMI_NAME" --query 'Images[0].ImageId' --output text 2>/dev/null \
     | grep -q '^ami-'; then
  die "$AMI_NAME already exists. AMIs are immutable — bump AMI_VERSION instead of rebuilding a version something may be running."
fi

# ---------------------------------------------------------------------------
# Inputs the builder needs. They go to S3 rather than into UserData: UserData is
# capped at 16 KB, and the provisioning scripts plus the drain agent exceed it.
# The builder reads them with the instance profile it already has.
# ---------------------------------------------------------------------------
BUCKET="$(aws cloudformation describe-stacks --stack-name "$PROJECT-dev-04-storage" --region "$REGION" \
  --query "Stacks[0].Outputs[?OutputKey=='ArtifactBucketName'].OutputValue" --output text 2>/dev/null)"
[[ -n "$BUCKET" && "$BUCKET" != "None" ]] || die "artifact bucket not found — deploy 04-storage first"

PROFILE="$(aws cloudformation describe-stacks --stack-name "$PROJECT-dev-02-iam" --region "$REGION" \
  --query "Stacks[0].Outputs[?OutputKey=='InstanceProfileName'].OutputValue" --output text 2>/dev/null)"
[[ -n "$PROFILE" && "$PROFILE" != "None" ]] || die "instance profile not found — deploy 02-iam first"

SUBNET="$(aws cloudformation describe-stacks --stack-name "$PROJECT-dev-01-network" --region "$REGION" \
  --query "Stacks[0].Outputs[?OutputKey=='PublicSubnetId'].OutputValue" --output text 2>/dev/null)"
SG="$(aws cloudformation describe-stacks --stack-name "$PROJECT-dev-01-network" --region "$REGION" \
  --query "Stacks[0].Outputs[?OutputKey=='InstanceSecurityGroupId'].OutputValue" --output text 2>/dev/null)"
[[ -n "$SUBNET" && "$SUBNET" != "None" ]] || die "subnet not found — deploy 01-network first"

S3_PREFIX="ami-build/v$AMI_VERSION"
say "inputs       : s3://$BUCKET/$S3_PREFIX/"
aws s3 cp "$AMI_DIR" "s3://$BUCKET/$S3_PREFIX/" --recursive --region "$REGION" --only-show-errors \
  --exclude "*" --include "*.sh" --include "*.service"

BASE_AMI="$(aws ssm get-parameter --region "$REGION" \
  --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 \
  --query 'Parameter.Value' --output text)"
say "base ami     : $BASE_AMI (latest Amazon Linux 2023)"

# ---------------------------------------------------------------------------
# Phase 1 — launch the builder and provision it.
#
# UserData stays tiny: fetch the scripts, run them. Everything of substance is in
# provision.sh, which is version-controlled and reviewable, rather than smuggled
# into a template as a string.
# ---------------------------------------------------------------------------
user_data="$(cat <<EOF
#!/bin/bash
set -euo pipefail
mkdir -p /tmp/ami
aws s3 sync "s3://$BUCKET/$S3_PREFIX/" /tmp/ami/ --region $REGION
chmod +x /tmp/ami/*.sh
GO_VERSION=$GO_VERSION NODE_MAJOR=$NODE_MAJOR COMPOSE_VERSION=$COMPOSE_VERSION \
  INSTALL_OLLAMA=$INSTALL_OLLAMA bash /tmp/ami/provision.sh
EOF
)"

printf '\nlaunching the builder\n'
BUILDER="$(aws ec2 run-instances --region "$REGION" \
  --image-id "$BASE_AMI" \
  --instance-type "$BUILDER_TYPE" \
  --subnet-id "$SUBNET" \
  --security-group-ids "$SG" \
  --iam-instance-profile "Name=$PROFILE" \
  --metadata-options "HttpTokens=required,HttpEndpoint=enabled" \
  --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=30,VolumeType=gp3,Encrypted=true,DeleteOnTermination=true}' \
  --user-data "$user_data" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$PROJECT-ami-builder},{Key=Project,Value=$PROJECT},{Key=Component,Value=ami-builder},{Key=Ephemeral,Value=true}]" \
  --query 'Instances[0].InstanceId' --output text)"
say "builder      : $BUILDER"

# The builder is a throwaway that costs money and holds an IAM role. It must not
# survive this script under any exit path — including a failure, an unset
# variable, or a Ctrl-C halfway through a ten-minute build.
cleanup_builder() {
  local code=$?
  if [[ -n "${BUILDER:-}" ]]; then
    printf '\nterminating the builder (%s)\n' "$BUILDER"
    aws ec2 terminate-instances --region "$REGION" --instance-ids "$BUILDER" >/dev/null 2>&1 || true
  fi
  exit $code
}
trap cleanup_builder EXIT INT TERM

say "waiting for it to come up"
aws ec2 wait instance-running --region "$REGION" --instance-ids "$BUILDER"

say "waiting for SSM"
for _ in $(seq 1 60); do
  [[ "$(aws ssm describe-instance-information --region "$REGION" \
        --filters "Key=InstanceIds,Values=$BUILDER" \
        --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null)" == "Online" ]] && break
  sleep 5
done

# Poll for the marker provision.sh plants. This is the only trustworthy signal
# that the build worked: the instance being "running" says nothing about whether
# dnf succeeded.
printf '\nprovisioning (this is the slow part — it is why the AMI exists)\n'
started=$(date +%s)
while :; do
  elapsed=$(( $(date +%s) - started ))
  (( elapsed < BUILD_TIMEOUT_SECONDS )) || die "provision did not finish within ${BUILD_TIMEOUT_SECONDS}s"

  out="$(aws ssm send-command --region "$REGION" --instance-ids "$BUILDER" \
    --document-name AWS-RunShellScript \
    --parameters 'commands=["if [ -f /var/lib/ami-build.failed ]; then echo FAILED; cat /var/lib/ami-build.failed; tail -30 /var/log/ami-build.log; elif [ -f /var/lib/ami-build.done ]; then echo DONE; else tail -1 /var/log/ami-build.log 2>/dev/null || echo starting; fi"]' \
    --query 'Command.CommandId' --output text 2>/dev/null || true)"

  if [[ -n "$out" ]]; then
    sleep 6
    result="$(aws ssm get-command-invocation --region "$REGION" --command-id "$out" \
      --instance-id "$BUILDER" --query 'StandardOutputContent' --output text 2>/dev/null || true)"
    case "$result" in
      DONE*)   printf '  provisioned in %ss\n' "$elapsed"; break ;;
      FAILED*) printf '\n%s\n' "$result"; die "provision failed on the builder" ;;
      *)       [[ -n "${result// }" ]] && printf '  [%3ss] %s\n' "$elapsed" "$(echo "$result" | tail -1 | cut -c1-90)" ;;
    esac
  fi
  sleep 15
done

# Capture the manifest before cleanup truncates the logs — it is what tells you,
# six weeks later, what is actually inside this image.
cmd="$(aws ssm send-command --region "$REGION" --instance-ids "$BUILDER" \
  --document-name AWS-RunShellScript --parameters 'commands=["cat /etc/ami-manifest.json"]' \
  --query 'Command.CommandId' --output text)"
sleep 6
MANIFEST="$(aws ssm get-command-invocation --region "$REGION" --command-id "$cmd" \
  --instance-id "$BUILDER" --query 'StandardOutputContent' --output text 2>/dev/null || echo '{}')"
printf '\nbaked:\n'
echo "$MANIFEST" | jq -r 'to_entries[] | "  \(.key): \(.value)"' 2>/dev/null || echo "$MANIFEST"

# ---------------------------------------------------------------------------
# Phase 2 — harden and shut down.
#
# The SSM command will not report success: cleanup stops the SSM agent and then
# powers the instance off, so the invocation dies with it. That is expected. The
# instance reaching "stopped" IS the success signal.
# ---------------------------------------------------------------------------
printf '\nhardening and shutting down\n'
aws ssm send-command --region "$REGION" --instance-ids "$BUILDER" \
  --document-name AWS-RunShellScript \
  --parameters 'commands=["nohup bash /tmp/ami/cleanup.sh > /dev/null 2>&1 &"]' \
  --query 'Command.CommandId' --output text >/dev/null

say "waiting for the builder to stop (its clean shutdown quiesces the filesystem)"
aws ec2 wait instance-stopped --region "$REGION" --instance-ids "$BUILDER"

# ---------------------------------------------------------------------------
# Snapshot. The instance is stopped, so the image is consistent without needing
# --no-reboot (which snapshots a live filesystem and can capture it mid-write).
# ---------------------------------------------------------------------------
printf '\ncreating the image\n'
AMI_ID="$(aws ec2 create-image --region "$REGION" \
  --instance-id "$BUILDER" \
  --name "$AMI_NAME" \
  --description "AI Agent Platform base image v$AMI_VERSION (go=$GO_VERSION node=$NODE_MAJOR ollama=$INSTALL_OLLAMA)" \
  --query 'ImageId' --output text)"
say "ami          : $AMI_ID"

# Tags are the interface. The compute stack finds the image to launch by asking
# for the newest AMI tagged with this project and component, so these tags are
# not documentation — they are how the pipeline is wired together.
aws ec2 create-tags --region "$REGION" --resources "$AMI_ID" --tags \
  "Key=Name,Value=$AMI_NAME" \
  "Key=Project,Value=$PROJECT" \
  "Key=Component,Value=platform-ami" \
  "Key=Version,Value=$AMI_VERSION" \
  "Key=BaseAmi,Value=$BASE_AMI" \
  "Key=BuiltAt,Value=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  "Key=Ollama,Value=$INSTALL_OLLAMA" \
  "Key=ManagedBy,Value=build-ami.sh" \
  "Key=Milestone,Value=4"

say "waiting for the image to become available"
aws ec2 wait image-available --region "$REGION" --image-ids "$AMI_ID"

# Tag the snapshot too. An untagged snapshot is an untraceable line on a bill,
# and snapshots are where an AMI's storage cost actually lives.
for snap in $(aws ec2 describe-images --region "$REGION" --image-ids "$AMI_ID" \
                --query 'Images[0].BlockDeviceMappings[].Ebs.SnapshotId' --output text); do
  aws ec2 create-tags --region "$REGION" --resources "$snap" --tags \
    "Key=Name,Value=$AMI_NAME" "Key=Project,Value=$PROJECT" \
    "Key=Component,Value=platform-ami" "Key=Version,Value=$AMI_VERSION" >/dev/null
done

printf '\ndone.\n\n'
say "$AMI_NAME"
say "$AMI_ID"
printf '\ndeploy it:\n'
say "make -C infra deploy-03-compute AMI_ID=$AMI_ID"
printf '\n'
