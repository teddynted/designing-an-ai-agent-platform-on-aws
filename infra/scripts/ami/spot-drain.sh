#!/bin/bash
# Watch for a Spot interruption notice; drain the instance when it comes.
#
# This is the on-host half of Spot interruption handling (Milestone 3). AWS gives
# roughly 120 seconds between the notice and the reclaim, and only something
# running on the instance can stop the workload and get its output off a disk
# that is about to be deleted. See infra/SPOT.md.
#
# THIS FILE IS THE SINGLE SOURCE OF TRUTH for the agent. It is:
#   * baked into the custom AMI (scripts/ami/provision.sh), and
#   * embedded verbatim in the traditional UserData (cloudformation/03-compute.yaml)
#     for instances launched without a custom AMI.
# `make check-drain-sync` fails if those two drift apart.
#
# It contains NO project- or environment-specific values, deliberately: an AMI is
# shared across dev, staging and prod, so everything that varies between them is
# configuration, read at boot from /etc/spot-drain.env, and nothing that varies is
# code. That is the whole boundary between what may be baked and what may not.
set -uo pipefail

. /etc/spot-drain.env

IMDS="http://169.254.169.254"

log() {
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) spot-drain: $*" >> "$DRAIN_LOG"
}

# IMDSv2: every read needs a session token first.
imds_token() {
  curl -sf -X PUT "$IMDS/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 300"
}

imds_get() {
  curl -sf -H "X-aws-ec2-metadata-token: $1" "$IMDS/latest/meta-data/$2"
}

# Everything that must happen before the instance disappears, in the order it
# must happen in. There is no second attempt: whatever is not done when AWS
# reclaims the instance is not done at all.
drain() {
  notice="$1"
  instance_id="$2"
  log "interruption notice: $notice"

  # 1. Stop the workloads, so nothing is still writing while we upload.
  #    Unquoted on purpose: the units are one space-separated string, and the
  #    default guards against the agent dying on `set -u` if the variable is ever
  #    absent. Nothing here may abort the drain.
  for unit in ${DRAIN_UNITS:-}; do
    log "stopping $unit"
    systemctl stop "$unit" || log "WARNING: could not stop $unit"
  done

  # 2. Save the work. This is the entire reason the agent exists: the instance is
  #    disposable, its output is not.
  if [ -d "$ARTIFACT_DIR" ] && [ -n "$(ls -A "$ARTIFACT_DIR" 2>/dev/null)" ]; then
    log "saving artifacts to s3://$ARTIFACT_BUCKET/drain/$instance_id/"
    aws s3 sync "$ARTIFACT_DIR" \
      "s3://$ARTIFACT_BUCKET/drain/$instance_id/artifacts/" --only-show-errors \
      && log "artifacts saved" \
      || log "ERROR: artifact sync failed"
  else
    log "no artifacts to save"
  fi

  # 3. Leave a marker. Without it, a post-mortem cannot tell an instance that
  #    drained cleanly from one that simply vanished.
  echo "$notice" > /tmp/interruption.json
  aws s3 cp /tmp/interruption.json \
    "s3://$ARTIFACT_BUCKET/drain/$instance_id/interruption.json" --only-show-errors \
    || log "ERROR: could not upload the interruption marker"

  log "drain complete"
}

log "drain agent started; polling every $POLL_SECONDS seconds"

while true; do
  token="$(imds_token)" || { sleep "$POLL_SECONDS"; continue; }

  # 404 until AWS issues a notice — the healthy path, every time.
  notice="$(imds_get "$token" spot/instance-action)" || {
    sleep "$POLL_SECONDS"
    continue
  }

  instance_id="$(imds_get "$token" instance-id)"
  drain "$notice" "$instance_id"
  exit 0
done
