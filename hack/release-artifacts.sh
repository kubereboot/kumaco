#!/bin/sh
# Renders what a release attaches besides its binaries and images (AGENTS.md,
# "Versioning and release policy"): the CRD, the kustomize manifests with the
# image of the release, and the Helm chart. GoReleaser runs it before a build.
#
#   hack/release-artifacts.sh <version> <tag> <image repository>
set -eu

version="$1"
tag="$2"
image="$3"
out=.tmp/release

rm -rf "$out"
mkdir -p "$out/kustomize"

cp config/crd/bases/kumaco.kured.dev_maintenancewindows.yaml "$out/kumaco-crd.yaml"

cat >"$out/kustomize/kustomization.yaml" <<KUSTOMIZATION
resources:
  - ../../../config/default
images:
  - name: ghcr.io/kubereboot/kumaco
    newName: $image
    newTag: $tag
KUSTOMIZATION
kustomize build "$out/kustomize" >"$out/kumaco.yaml"
rm -r "$out/kustomize"

# The chart is published with the release it deploys: its appVersion is the
# tag of the image.
helm package charts/kumaco --version "$version" --app-version "$tag" --destination "$out" >/dev/null
