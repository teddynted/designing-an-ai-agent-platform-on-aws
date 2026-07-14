#!/bin/bash
# Harden the builder immediately before its disk becomes an AMI, then shut it
# down so the snapshot is taken from a quiesced filesystem.
#
# This is the part that is easiest to skip and most expensive to skip. An AMI is
# a filesystem that can be copied to another account, shared, or made public with
# a single API call — and unlike a running instance, nobody is watching it.
# Whatever is on this disk right now is on the disk of every instance ever
# launched from this image, forever, and in every account it is ever shared with.
#
# Run as a second phase, by build-ami.sh over SSM, once provision.sh has been
# confirmed to have succeeded. It is not part of provision.sh because it destroys
# the two things provision.sh is standing on: cloud-init's copy of the running
# script, and the SSM agent that reports the result.
set -uo pipefail   # NOT -e: a failed cleanup step must not skip the shutdown

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) ami-cleanup: $*" | tee -a /var/log/ami-build.log; }

log "hardening the image"

# --- 1. Credentials the build itself created -------------------------------
# The builder had an instance profile, and the SDK and CLI cache its credentials
# on disk. Those are short-lived, but "short-lived" is not "gone", and they have
# no business being in an image.
rm -rf /root/.aws /home/ec2-user/.aws
rm -f  /root/.docker/config.json /home/ec2-user/.docker/config.json

# --- 2. Shell history -------------------------------------------------------
# Build scripts routinely echo things they should not, and history outlives the
# terminal that produced it.
rm -f /root/.bash_history /home/ec2-user/.bash_history

# --- 3. SSH identity --------------------------------------------------------
# Any key authorised on the builder would be authorised on every instance
# launched from this image. And host keys must be per-instance: bake them and
# every instance presents the same host identity, which quietly defeats the
# entire point of host key verification.
rm -f /home/ec2-user/.ssh/authorized_keys /root/.ssh/authorized_keys
rm -f /etc/ssh/ssh_host_*

# --- 4. Build residue -------------------------------------------------------
# Package caches are pure image size — and image size is snapshot cost, paid
# monthly, on every AMI version you retain. The logs are the *builder's*, which
# are worse than useless on a fresh instance: they are misleading.
dnf clean all
rm -rf /var/cache/dnf /var/cache/yum /tmp/ami
find /var/log -type f -exec truncate -s 0 {} \; 2>/dev/null

# --- 5. Machine identity ----------------------------------------------------
# machine-id must be unique per instance. Bake one and every instance from this
# image has the same identity to systemd, journald, and anything keyed on it.
truncate -s 0 /etc/machine-id

# --- 6. SSM registration ----------------------------------------------------
# The agent registered the BUILDER as a managed instance. Left in the image,
# every instance launched from it claims to be that same builder, and they fight
# over the identity in Session Manager.
#
# Stop the agent first: delete its state while it is running and it simply writes
# it back.
systemctl stop amazon-ssm-agent 2>/dev/null
rm -rf /var/lib/amazon/ssm/Vault/Store/RegistrationKey \
       /var/lib/amazon/ssm/registration \
       /var/lib/amazon/ssm/ipc

# --- 7. THE ONE THAT BITES EVERYONE ----------------------------------------
# cloud-init records, on disk, that it has already run for this instance. Bake
# that state and cloud-init on the new instance concludes it has nothing to do —
# and UserData NEVER RUNS. The instance boots, reports healthy, passes its status
# checks, and is completely unconfigured: no drain agent, no log group, nothing.
#
# It is a silent failure. There is no error anywhere. This one line is the
# difference between an AMI that works and an AMI that only appears to.
cloud-init clean --logs --seed
rm -rf /var/lib/cloud/instances /var/lib/cloud/data

log "hardened; shutting down for the snapshot"

# Shut down rather than let build-ami.sh stop us: the filesystem is quiesced by a
# clean shutdown, and reaching the "stopped" state is the signal build-ami.sh
# waits on — the SSM agent is dead by now and cannot report anything else.
sync
shutdown -h now
