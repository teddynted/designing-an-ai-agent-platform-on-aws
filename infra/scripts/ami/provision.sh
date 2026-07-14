#!/bin/bash
# Provision the AI Agent Platform's custom AMI.
#
# This runs ONCE, on a throwaway builder instance, and everything it installs is
# frozen into an image that every future instance boots from already complete.
# Work done here is paid for once, at build time, by a machine nobody is waiting
# for — instead of on every launch, by a Spot instance that may be reclaimed in
# two minutes.
#
# THE RULE FOR WHAT GOES IN HERE
#
#   Bake what is the same everywhere. Configure what differs.
#
# One AMI is shared by dev, staging and prod. So anything that varies between
# environments — a bucket name, an event bus, a project prefix, a schedule — is
# NOT baked; it is written at boot by UserData. Anything that is identical
# everywhere — a Docker binary, a Go toolchain, the drain agent's code — is baked.
# Get that boundary wrong and you have either an AMI that only works in one
# environment, or a boot that still has to think.
#
# AND WHAT NEVER GOES IN HERE
#
# Never bake a secret. An AMI is a filesystem that can be copied, shared across
# accounts, and made public by one careless API call — and unlike a running
# instance, nobody is watching it. No credentials, no tokens, no private keys, no
# .env files. Identity comes from the instance profile at runtime, which is
# rotated by AWS and scoped by IAM. See `harden()` at the bottom: it removes the
# credentials the build itself created.
#
# Invoked by build-ami.sh as the builder instance's UserData.
set -euo pipefail

# Pinned, so that rebuilding the same AMI version twice produces the same image.
# An unpinned "latest" is how two builds of "v3" end up with different Go
# compilers and one mysteriously fails.
GO_VERSION="${GO_VERSION:-1.23.4}"
NODE_MAJOR="${NODE_MAJOR:-20}"
COMPOSE_VERSION="${COMPOSE_VERSION:-v2.32.1}"
INSTALL_OLLAMA="${INSTALL_OLLAMA:-false}"

BUILD_LOG=/var/log/ami-build.log
DONE_MARKER=/var/lib/ami-build.done
MANIFEST=/etc/ami-manifest.json

exec > >(tee -a "$BUILD_LOG") 2>&1

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) ami-build: $*"; }

# A build that half-succeeds is worse than one that fails: it produces an image
# that looks fine and is missing something. Record the failure where build-ami.sh
# will find it, and stop.
trap 'echo "FAILED at line $LINENO" > /var/lib/ami-build.failed' ERR

log "starting provision (go=$GO_VERSION node=$NODE_MAJOR compose=$COMPOSE_VERSION ollama=$INSTALL_OLLAMA)"

# ---------------------------------------------------------------------------
# 1. Base system
#
# The patches go in the image. Every instance that boots from it is already up to
# date, and nobody pays `dnf update` again — which on the traditional path is the
# single slowest thing in the boot, and the one most likely to fail.
# ---------------------------------------------------------------------------
log "updating the base system"
dnf -y update

log "installing base packages"
dnf -y install \
  git \
  jq \
  unzip \
  tar \
  gzip \
  python3 \
  python3-pip \
  amazon-cloudwatch-agent

# ---------------------------------------------------------------------------
# 2. Container runtime — Docker and the Compose plugin.
#
# The agent workloads (OpenClaw, n8n) ship as containers, so the runtime is a
# platform dependency, not an application one. Baking it also bakes the thing
# that most often breaks a boot: pulling a package from the internet.
# ---------------------------------------------------------------------------
log "installing docker"
dnf -y install docker
systemctl enable docker

log "installing the docker compose plugin $COMPOSE_VERSION"
install -d -m 0755 /usr/libexec/docker/cli-plugins
curl -fsSL --retry 3 \
  "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-$(uname -m)" \
  -o /usr/libexec/docker/cli-plugins/docker-compose
chmod 0755 /usr/libexec/docker/cli-plugins/docker-compose

# ec2-user runs containers without sudo. Harmless here; the instance has no
# inbound access and is reached only through SSM.
usermod -aG docker ec2-user

# ---------------------------------------------------------------------------
# 3. Language runtimes.
#
# Go, because this platform's own tooling is Go. Node, because n8n is Node.
# Python, for the AI/ML ecosystem (boto3, the embedding and inference clients a
# later milestone will use).
# ---------------------------------------------------------------------------
log "installing go $GO_VERSION"
arch="$(uname -m)"; go_arch="amd64"; [ "$arch" = "aarch64" ] && go_arch="arm64"
curl -fsSL --retry 3 "https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz" -o /tmp/go.tgz
rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
# In the image, not in a login script: a systemd service is not a login shell and
# would never see a ~/.bashrc export.
printf 'export PATH=$PATH:/usr/local/go/bin\n' > /etc/profile.d/go.sh
chmod 0644 /etc/profile.d/go.sh
ln -sf /usr/local/go/bin/go /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

log "installing node $NODE_MAJOR"
dnf -y install "nodejs${NODE_MAJOR}" "nodejs${NODE_MAJOR}-npm" || dnf -y install nodejs npm

# boto3 only, and deliberately without --upgrade. `requests` is already present as
# an RPM, and pip cannot uninstall an RPM-managed package: it fails with "Cannot
# uninstall requests … RECORD file not found" and takes the whole build with it.
# Let dnf own what dnf installed; let pip add what dnf does not ship.
log "installing python packages"
pip3 install --no-cache-dir boto3

# ---------------------------------------------------------------------------
# 4. Ollama (optional).
#
# Off by default, and the default is the honest one: Ollama adds ~1.5 GB to the
# image, which is snapshot cost paid every month, on every AMI version retained,
# for a binary this milestone does not yet run. Turn it on when Milestone 7
# actually serves inference — the image is where it belongs, because pulling a
# 1.5 GB binary at boot is exactly the tax custom AMIs exist to remove.
#
# NOTE: this bakes the Ollama *binary*, never a *model*. Models are gigabytes,
# change independently of the OS, and belong on S3 or a volume — bake one and
# every AMI version carries a frozen copy of it.
# ---------------------------------------------------------------------------
if [ "$INSTALL_OLLAMA" = "true" ]; then
  log "installing ollama"
  curl -fsSL --retry 3 https://ollama.com/install.sh | sh
  systemctl disable ollama || true   # started by UserData, not by the image
else
  log "skipping ollama (INSTALL_OLLAMA=false)"
fi

# ---------------------------------------------------------------------------
# 5. The Spot drain agent (Milestone 3).
#
# The agent's *code* is identical in every environment, so it is baked. Its
# *configuration* — bucket, log path, which units to stop — differs per
# environment, so UserData writes /etc/spot-drain.env at boot. The unit has
# ConditionPathExists on that file, so a baked-but-unconfigured image simply does
# not start it.
#
# On the traditional path this same script had to be written out by UserData on
# every single launch. Now it is already there.
# ---------------------------------------------------------------------------
log "installing the spot drain agent"
install -m 0755 /tmp/ami/spot-drain.sh /usr/local/bin/spot-drain
install -m 0644 /tmp/ami/spot-drain.service /etc/systemd/system/spot-drain.service
systemctl enable spot-drain.service    # enabled now, started at boot once configured

# ---------------------------------------------------------------------------
# 6. CloudWatch agent.
#
# Installed and configured here; pointed at an environment-specific log group by
# UserData, which drops in /opt/aws/amazon-cloudwatch-agent/etc/log-group.conf.
# The agent ships the boot log and the drain log, which are the two things you
# want when an instance is gone and cannot be asked what happened.
# ---------------------------------------------------------------------------
log "configuring the cloudwatch agent"
install -d -m 0755 /opt/aws/amazon-cloudwatch-agent/etc
cat > /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json <<'CWA'
{
  "agent": { "run_as_user": "root" },
  "logs": {
    "logs_collected": {
      "files": {
        "collect_list": [
          {
            "file_path": "/var/log/cloud-init-output.log",
            "log_group_name": "${LOG_GROUP}",
            "log_stream_name": "{instance_id}/cloud-init",
            "retention_in_days": -1
          },
          {
            "file_path": "/var/log/spot-drain.log",
            "log_group_name": "${LOG_GROUP}",
            "log_stream_name": "{instance_id}/spot-drain",
            "retention_in_days": -1
          }
        ]
      }
    }
  }
}
CWA
# Not enabled here: the agent would start before UserData has told it which log
# group to write to. UserData substitutes ${LOG_GROUP} and starts it.

# ---------------------------------------------------------------------------
# 7. Filesystem contract.
#
# The directory the drain agent saves to. Baked so it always exists, with the
# right ownership, before any workload looks for it.
# ---------------------------------------------------------------------------
install -d -m 0755 /var/lib/ai-platform/artifacts
chown ec2-user:ec2-user /var/lib/ai-platform/artifacts

# ---------------------------------------------------------------------------
# 8. Manifest.
#
# What is actually in this image, written into the image. When an instance
# misbehaves six weeks from now, "which Go is on it?" should be answerable by
# reading a file, not by guessing from the AMI name.
# ---------------------------------------------------------------------------
log "writing the manifest"
cat > "$MANIFEST" <<MANIFEST_JSON
{
  "builtAt": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "baseAmi": "$(curl -sf -H "X-aws-ec2-metadata-token: $(curl -sf -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')" http://169.254.169.254/latest/meta-data/ami-id || echo unknown)",
  "kernel": "$(uname -r)",
  "docker": "$(docker --version 2>/dev/null | head -1)",
  "dockerCompose": "$(/usr/libexec/docker/cli-plugins/docker-compose version --short 2>/dev/null || echo none)",
  "go": "$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}')",
  "node": "$(node --version 2>/dev/null || echo none)",
  "python": "$(python3 --version 2>/dev/null | awk '{print $2}')",
  "git": "$(git --version 2>/dev/null | awk '{print $3}')",
  "cloudwatchAgent": "$(rpm -q amazon-cloudwatch-agent 2>/dev/null || echo none)",
  "ollama": "$(ollama --version 2>/dev/null | tail -1 || echo none)"
}
MANIFEST_JSON
chmod 0644 "$MANIFEST"
cat "$MANIFEST"

# ---------------------------------------------------------------------------
# 9. Done.
#
# Hardening and cleanup are deliberately NOT here — they run as a second phase
# (cleanup.sh), driven by build-ami.sh once this phase is confirmed to have
# succeeded. Two reasons, both learned the hard way:
#
#   * `cloud-init clean` deletes the very script cloud-init is running. Bash
#     reads a script incrementally, so deleting it mid-execution can truncate the
#     rest of the build.
#   * cleanup stops the SSM agent, which is the channel used to verify that this
#     phase worked at all. Kill it here and the build has no way to report.
#
# So this phase ends by planting a marker, and stops. build-ami.sh reads the
# marker over SSM, and only then triggers cleanup.
# ---------------------------------------------------------------------------
log "provision complete"
touch "$DONE_MARKER"
