#!/usr/bin/env bash
# Runs the e2e scenarios on a clean kind cluster per Argo CD version.
#   run.sh current|previous|all  small|full
# Prints runtime and the peak memory of the kind node container per version.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
# shellcheck disable=SC1091
source "$here/versions.env"

which="${1:?usage: run.sh current|previous|all small|full}"
set_="${2:?usage: run.sh current|previous|all small|full}"

case "$which" in
current) versions=("$ARGO_CURRENT") ;;
previous) versions=("$ARGO_PREVIOUS") ;;
all) versions=("$ARGO_PREVIOUS" "$ARGO_CURRENT") ;;
*)
  echo "unknown version selector: $which" >&2
  exit 2
  ;;
esac

# Peak memory (MiB) of the node container, sampled every 5 s until the file $1 disappears.
sample_mem() {
  local node="$1" flag="$2" peak=0 raw mib
  while [ -e "$flag" ]; do
    raw="$(docker stats --no-stream --format '{{.MemUsage}}' "$node" 2>/dev/null | cut -d/ -f1 | tr -d ' ' || true)"
    case "$raw" in
    *GiB) mib="$(awk -v v="${raw%GiB}" 'BEGIN{printf "%d", v*1024}')" ;;
    *MiB) mib="$(awk -v v="${raw%MiB}" 'BEGIN{printf "%d", v}')" ;;
    *) mib=0 ;;
    esac
    if [ "${mib:-0}" -gt "$peak" ]; then peak="$mib"; fi
    echo "$peak" >"$flag.peak"
    sleep 5
  done
}

rc=0
for ver in "${versions[@]}"; do
  tag="${ver//./-}"
  node="ase-${tag}-control-plane"
  start="$(date +%s)"
  cleanup() { "$here/cluster.sh" down "$ver" >/dev/null 2>&1 || true; }
  trap cleanup EXIT
  "$here/cluster.sh" up "$ver"
  flag="$(mktemp)"
  sample_mem "$node" "$flag" &
  sampler=$!
  set +e
  (
    cd "$here"
    ASE_E2E_VERSION="$ver" ASE_E2E_SET="$set_" ASE_BIN="$root/bin/argocd-sync-explain" \
      go test -tags e2e -count=1 -v -timeout 40m ./...
  )
  status=$?
  set -e
  rm -f "$flag"
  wait "$sampler" 2>/dev/null || true
  peak="$(cat "$flag.peak" 2>/dev/null || echo unknown)"
  rm -f "$flag.peak"
  end="$(date +%s)"
  echo "RESULT version=$ver set=$set_ status=$status runtime_s=$((end - start)) peak_node_mem_mib=$peak"
  cleanup
  trap - EXIT
  if [ "$status" -ne 0 ]; then rc=1; fi
done
exit "$rc"
