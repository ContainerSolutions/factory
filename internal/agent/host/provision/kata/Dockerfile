# The kata driver's workspace image (design/workstreams/agent/LLD/workspace-kata.md, "The image"):
# Ubuntu 24.04 with systemd as PID 1 and the tools of the same base.sh the tart base VM runs,
# in its image mode. factory-agent writes this build context (provision.WriteKataContext: this
# file, base.sh and the three below, side by side) and builds it with BuildKit; CI builds the
# same context with docker build (make kata-context).
FROM docker.io/library/ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

# What systemd as PID 1, base.sh and the storage unit need. admin (uid 1000, passwordless sudo)
# is the cirruslabs images' user, which the workspace runs as; the image's own uid 1000,
# ubuntu, goes first.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        systemd systemd-sysv dbus sudo iproute2 kmod e2fsprogs util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && userdel -r ubuntu \
    && useradd -m -u 1000 -s /bin/bash -G sudo admin \
    && echo 'admin ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/admin \
    && chmod 0440 /etc/sudoers.d/admin

# base.sh runs as admin through sudo, as in the tart base, so the files it makes for admin
# (the gh extensions under ~/.local) are owned the same way. It is mounted for the one step,
# not copied, so it isn't left in the image.
USER admin
RUN --mount=type=bind,source=base.sh,target=/tmp/base.sh \
    FACTORY_BASE_MODE=image bash /tmp/base.sh
USER root

COPY --chmod=0755 factory-init factory-docker-storage /usr/local/sbin/
COPY --chmod=0644 factory-docker-storage.service /etc/systemd/system/

# dockerd's storage before docker and containerd; nothing that manages consoles or devices
# (base.sh masked the apt timers). Each workspace's systemd makes its own machine-id.
RUN systemctl enable factory-docker-storage.service docker.service containerd.service \
    && systemctl mask getty.target console-getty.service systemd-udevd.service systemd-udev-trigger.service \
    && truncate -s 0 /etc/machine-id

# systemd halts on SIGRTMIN+3: kata-helper stop's signal.
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/local/sbin/factory-init"]
