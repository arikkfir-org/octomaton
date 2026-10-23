#!/bin/sh
# Renders deploy/ and deploy/logs/ as written into OUT, and validates every resource with kubeconform: built-in kinds
# only, so the default schemas cover them. How a host renders them is the host's to check (the README's Hosting
# Octomaton). Runs in docker.io/alpine/k8s, whose kustomize is Argo CD's.
#
#   deploy/manifests.sh OUT
set -eu

out="${1:?usage: deploy/manifests.sh OUT}"
mkdir -p "${out}"

kustomize build deploy > "${out}/deploy.yaml"
echo "deploy/ as written: ${out}/deploy.yaml"
kustomize build deploy/logs > "${out}/logs.yaml"
echo "deploy/logs/ as written: ${out}/logs.yaml"

kubeconform -strict -summary -schema-location default "${out}"
