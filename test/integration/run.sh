#!/bin/sh
# Integration test: a real `footops install` in a systemd container with Docker inside,
# then the scenario scripts. Runs against any Docker; developed with Colima on macOS.
#
#   test/integration/run.sh            install (or update) and run all scenarios
#   test/integration/run.sh clean      remove the container and its volumes
#   docker stop footops-it             free the memory between runs (run.sh restarts it)
#
# The container is kept between runs so a second run tests the update path and is
# fast. Colima's default VM has 2 GB RAM and no swap: add some first, e.g.
#   colima ssh -- sudo sh -c 'fallocate -l 2G /swap-it && chmod 600 /swap-it && mkswap /swap-it && swapon /swap-it'
# (remove it afterwards with swapoff /swap-it && rm /swap-it).
set -eu
cd "$(dirname "$0")"
NAME=footops-it
IMAGE=footops-it:ubuntu24.04
WORK=${TMPDIR:-/tmp}/footops-it
mkdir -p "$WORK" && chmod 700 "$WORK"

if [ "${1:-}" = clean ]; then
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	docker volume rm footops-it-docker footops-it-containerd >/dev/null 2>&1 || true
	rm -rf "$WORK"
	echo "temizlendi"
	exit 0
fi

arch=$(docker info --format '{{.Architecture}}')
case $arch in
aarch64 | arm64) goarch=arm64 ;;
x86_64 | amd64) goarch=amd64 ;;
*) echo "bilinmeyen mimari: $arch" >&2; exit 1 ;;
esac
(cd ../.. && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath \
	-ldflags "-X github.com/swbilisim/FootOps/internal/version.Version=it-$(git rev-parse --short HEAD 2>/dev/null || echo dev)" \
	-o "$WORK/footops" ./cmd/footops)

docker build -q -t "$IMAGE" . >/dev/null
if ! docker inspect "$NAME" >/dev/null 2>&1; then
	docker run -d --name "$NAME" --hostname fo-it --privileged \
		--tmpfs /run --tmpfs /run/lock \
		-v footops-it-docker:/var/lib/docker -v footops-it-containerd:/var/lib/containerd \
		"$IMAGE" >/dev/null
	echo "container başlatıldı: $NAME"
elif [ "$(docker inspect -f '{{.State.Running}}' "$NAME")" != true ]; then
	docker start "$NAME" >/dev/null # kept from an earlier run, stopped to free memory
fi
until docker exec "$NAME" systemctl is-system-running 2>/dev/null | grep -Eq 'running|degraded'; do sleep 1; done

[ -f "$WORK/id_it" ] || ssh-keygen -q -t ed25519 -N '' -C footops-it -f "$WORK/id_it"
docker cp "$WORK/footops" "$NAME:/root/footops"
python3 install.py "$NAME" "$WORK/id_it.pub" "$WORK/login"
docker exec "$NAME" rm -f /root/footops
python3 f0.py "$NAME" "$WORK/login"
python3 f2.py "$NAME" "$WORK/login"
python3 f2b.py "$NAME" "$WORK/login"
python3 f3.py "$NAME" "$WORK/login"
python3 f4.py "$NAME" "$WORK/login"
