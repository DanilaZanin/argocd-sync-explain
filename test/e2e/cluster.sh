#!/usr/bin/env bash
# Creates and deletes the e2e kind cluster for one Argo CD version.
#   cluster.sh up   vX.Y.Z   create cluster ase-vX-Y-Z, install Argo CD and the in-cluster git server
#   cluster.sh down vX.Y.Z   delete that cluster
# Only clusters named ase-* are ever touched.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$here/versions.env"

cmd="${1:?usage: cluster.sh up|down VERSION}"
ver="${2:?usage: cluster.sh up|down VERSION}"
tag="${ver//./-}"
name="ase-${tag}"
vkey="${ver//./_}"
cache="$here/.cache"

case "$cmd" in
down)
  if kind get clusters 2>/dev/null | grep -qx "$name"; then
    kind delete cluster --name "$name"
  fi
  exit 0
  ;;
up) ;;
*)
  echo "unknown command: $cmd" >&2
  exit 2
  ;;
esac

sha_var="ARGO_SHA256_${vkey}"
img_var="ARGOCD_IMAGE_${vkey}"
want_sha="${!sha_var:?no pinned sha256 for $ver in versions.env}"
argo_image="${!img_var:?no pinned image for $ver in versions.env}"

mkdir -p "$cache"
manifest="$cache/install-$ver.yaml"
if [ ! -f "$manifest" ]; then
  curl -fsSL "https://raw.githubusercontent.com/argoproj/argo-cd/$ver/manifests/install.yaml" -o "$manifest"
fi
got_sha="$(shasum -a 256 "$manifest" | cut -d' ' -f1)"
if [ "$got_sha" != "$want_sha" ]; then
  echo "install.yaml checksum mismatch for $ver: $got_sha != $want_sha" >&2
  exit 1
fi

if kind get clusters 2>/dev/null | grep -qx "$name"; then
  echo "cluster $name already exists; delete it first with: $0 down $ver" >&2
  exit 1
fi

kind create cluster --name "$name" --image "$KIND_NODE_IMAGE" --wait 120s

ctx="kind-$name"
k() { kubectl --context "$ctx" "$@"; }

# `kind load docker-image` fails on multi-arch images with the containerd image store
# ("content digest ... not found"), so single-platform archives are loaded instead.
case "$(docker info --format '{{.Architecture}}')" in
aarch64 | arm64) platform=linux/arm64 ;;
*) platform=linux/amd64 ;;
esac
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
for img in "$argo_image" "$REDIS_IMAGE" "$GITEA_IMAGE" "$BUSYBOX_IMAGE"; do
  docker image inspect "$img" >/dev/null 2>&1 || docker pull --platform "$platform" "$img"
  docker save --platform "$platform" "$img" -o "$tmp/img.tar"
  kind load image-archive --name "$name" "$tmp/img.tar"
done

k create namespace argocd
k apply -n argocd --server-side --force-conflicts -f "$manifest"
# Not needed for the scenarios; saves memory on the shared Docker VM.
k -n argocd scale deployment argocd-dex-server argocd-applicationset-controller argocd-notifications-controller --replicas=0

k apply -f "$here/gitea.yaml"

k -n argocd rollout status deployment/argocd-server --timeout=300s
k -n argocd rollout status deployment/argocd-repo-server --timeout=300s
k -n argocd rollout status deployment/argocd-redis --timeout=300s
k -n argocd rollout status statefulset/argocd-application-controller --timeout=300s
k -n git rollout status deployment/gitea --timeout=300s
echo "cluster $name ready (context $ctx)"
