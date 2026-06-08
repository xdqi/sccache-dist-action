#!/usr/bin/env bash
# M0 spike: prove sccache-dist distributes over a tsnet mesh in the
# coordinator-forward topology, clearing the X-Real-IP /
# invalid_bearer_token_mismatched_address check.
#
# Topology (two isolated docker networks => real cross-node routing):
#   coord:  tsnet "<pfx>-coordinator" + sccache-dist scheduler(:10600) + client.
#           tsbridge exposes :10600 over tsnet, and forwards 127.0.0.1:10501 ->
#           tsnet <pfx>-worker-1:10501.
#   worker: tsnet "<pfx>-worker-1" + sccache-dist server (docker builder).
#           tsbridge exposes :10501 over tsnet, and forwards 127.0.0.1:10600 ->
#           tsnet <pfx>-coordinator:10600 (so server's scheduler_url reaches it).
#
# KEY TEST: server registers public_addr=127.0.0.1:10501 (coordinator-local); the
# client reaches it via the coordinator's forward. Confirm registration + a
# distributed compile with NO mismatched_address.
set -euo pipefail
cd "$(dirname "$0")"
: "${TS_AUTHKEY:?set TS_AUTHKEY}"
TOKEN="${TOKEN:-spike-shared-token}"
TAG="${TAG:-tag:ci}"
PFX="spk$$"
IMG=sccache-spike:latest

say(){ printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

say "build image"
docker build -q -f Dockerfile.node -t "$IMG" . >/dev/null

docker network create "${PFX}-cn" >/dev/null
docker network create "${PFX}-wn" >/dev/null
cleanup(){
  docker rm -f "${PFX}-coord" "${PFX}-worker" >/dev/null 2>&1 || true
  docker network rm "${PFX}-cn" "${PFX}-wn" >/dev/null 2>&1 || true
  docker ps -aq --filter "ancestor=aidanhs/busybox" | xargs -r docker rm -f >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Render configs on the host, then docker cp them in (avoids nested-heredoc traps)
render(){ sed -e "s|__TOKEN__|${TOKEN}|g" \
              -e "s|__SERVER_PUBLIC_ADDR__|127.0.0.1:10501|g" \
              -e "s|__SCHEDULER_URL__|http://127.0.0.1:10600|g" "$1"; }
render scheduler.conf > /tmp/${PFX}-scheduler.conf
render server.conf    > /tmp/${PFX}-server.conf
render client.config  > /tmp/${PFX}-client.config

say "start containers"
docker run -d --name "${PFX}-coord"  --network "${PFX}-cn" \
  -v /var/run/docker.sock:/var/run/docker.sock "$IMG" sleep infinity >/dev/null
docker run -d --name "${PFX}-worker" --network "${PFX}-wn" \
  -v /var/run/docker.sock:/var/run/docker.sock "$IMG" sleep infinity >/dev/null
docker cp /tmp/${PFX}-scheduler.conf "${PFX}-coord:/run/scheduler.conf"
docker cp /tmp/${PFX}-server.conf    "${PFX}-worker:/run/server.conf"
docker cp /tmp/${PFX}-client.config  "${PFX}-coord:/run/client.config"

say "coordinator: scheduler + tsbridge (expose :10600, forward 10501->worker)"
docker exec -d "${PFX}-coord" sh -c "SCCACHE_NO_DAEMON=1 sccache-dist scheduler --config /run/scheduler.conf > /tmp/sched.log 2>&1"
sleep 1
docker exec -d "${PFX}-coord" sh -c "tsbridge -hostname ${PFX}-coordinator -authkey '${TS_AUTHKEY}' -tags '${TAG}' -expose ':10600' -local-target 127.0.0.1:10600 -listen 127.0.0.1:10501 -target ${PFX}-worker-1:10501 > /tmp/tsb.log 2>&1"

say "worker: tsbridge (expose :10501, forward 10600->coord) + server"
docker exec -d "${PFX}-worker" sh -c "tsbridge -hostname ${PFX}-worker-1 -authkey '${TS_AUTHKEY}' -tags '${TAG}' -expose ':10501' -local-target 127.0.0.1:10501 -listen 127.0.0.1:10600 -target ${PFX}-coordinator:10600 > /tmp/tsb.log 2>&1"

say "wait for tsnet mesh to settle (15s)"
sleep 15

say "worker: start sccache-dist server"
docker exec -d "${PFX}-worker" sh -c "SCCACHE_NO_DAEMON=1 sccache-dist server --config /run/server.conf > /tmp/server.log 2>&1"
sleep 8
say "server log (look for register success vs mismatched_address)"
docker exec "${PFX}-worker" sh -c "tail -10 /tmp/server.log" || true

say "coordinator: configure client, dist-status, one distributed compile"
docker exec "${PFX}-coord" sh -c '
  mkdir -p /root/.config/sccache && cp /run/client.config /root/.config/sccache/config
  export SCCACHE_DIST_FALLBACK=0
  sccache --stop-server >/dev/null 2>&1 || true
  sccache --start-server
  echo "--- dist-status (num_servers should be 1) ---"
  for i in 1 2 3 4 5 6 7 8 9 10; do
    s=$(sccache --dist-status 2>/dev/null)
    echo "$s"
    echo "$s" | grep -q "\"num_servers\": *[1-9]" && break
    sleep 2
  done
  echo "int main(void){return 0;}" > /tmp/h.c
  sccache --zero-stats >/dev/null
  echo "--- compile (no fallback) ---"
  sccache gcc -c /tmp/h.c -o /tmp/h.o && echo "COMPILE_OK file=$(ls -la /tmp/h.o | awk "{print \$5}")" || echo "COMPILE_FAIL"
  echo "--- stats ---"
  sccache -s | grep -iE "successful distributed|failed distributed|compile requests executed"
'
rm -f /tmp/${PFX}-*.conf /tmp/${PFX}-*.config
