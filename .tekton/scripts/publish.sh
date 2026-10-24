#!/bin/sh
# Builds and pushes the image of the main package $1 to KO_DOCKER_REPO with ko (/tools/ko, from ko.sh), tagged with
# REVISION's short SHA, which is also the version the binary reports (.ko.yaml reads VERSION). ko's Google keychain
# authenticates as the step's Workload Identity.
set -eu
VERSION=$(printf '%s' "${REVISION}" | cut -c1-7)
export VERSION
echo "Publishing ${KO_DOCKER_REPO}:${VERSION}"
/tools/ko build --bare --tags "${VERSION}" "$1"
