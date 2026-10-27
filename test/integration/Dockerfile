# A throwaway "server" for integration tests: Ubuntu 24.04 with systemd as PID 1, so
# `footops install` runs exactly as on a real VM (apt, systemd units, Docker inside).
FROM ubuntu:24.04
ENV container=docker DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus ca-certificates curl sudo \
      openssh-server iproute2 iptables kmod openssl python3 \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && systemctl mask systemd-logind.service getty@tty1.service console-getty.service \
      systemd-udevd.service systemd-udev-trigger.service systemd-firstboot.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
