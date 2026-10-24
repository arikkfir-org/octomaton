#!/bin/sh
# Saves Go's caches in /go-cache as archive SAVE (main, pull-requests/<number>) of bucket arikkfir-octomaton-ci-cache,
# for the next runs (docs/octomaton/designs/ci-caches.md); an empty SAVE (a merge group's run) saves nothing. It first
# trims them to what Octomaton builds with: the build-cache entries this run used (go-cache-restore.sh dated the rest
# two hours back), and the modules go.sum names, unpacked, without their zips, which Go doesn't read once a module is
# unpacked, or their test data. A cache it can't trim or save costs later runs time, never this run its result. Runs
# in curlimages/curl, as the step's Workload Identity, in the checkout.
set -eu
if [ -z "${SAVE}" ]; then
  echo "Nothing to save: a merge group's run reads main's caches only"
  exit 0
fi
token="$(curl -fsS -H 'Metadata-Flavor: Google' \
  http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token |
  sed -n 's/.*"access_token" *: *"\([^"]*\)".*/\1/p')"
bucket=https://storage.googleapis.com/upload/storage/v1/b/arikkfir-octomaton-ci-cache
# go.sum's modules as the module cache spells them, a capital as ! and its lower case.
sed -n 's|^\([^ ]*\) \([^ /]*\) .*|\1@\2|p' go.sum | sed 's/[A-Z]/!&/g' | tr '[:upper:]' '[:lower:]' >/tmp/modules
cd /go-cache
if [ ! -d build ] || [ ! -d mod ]; then
  echo "Nothing to save as ${SAVE}/go.tar.gz"
  exit 0
fi
if [ -s /tmp/modules ] && ! (
  find build -path 'build/??/*' -type f ! -newer .restored -delete &&
    chmod -R u+w mod && rm -rf mod/cache/download/sumdb &&
    find mod/cache/download -name '*.zip' -delete &&
    find mod -path mod/cache -prune -o -type d -name testdata -prune -exec rm -rf {} + &&
    (cd mod && find . -path ./cache -prune -o -type d -name '*@*' -prune -print) |
    sed 's|^\./||' | grep -vxF -f /tmp/modules | (cd mod && xargs -r rm -rf)
); then
  echo "Couldn't trim Go's caches"
fi
# shellcheck disable=SC3040 # busybox's sh, curlimages/curl's, has pipefail
if ! (set -o pipefail; tar -cf - build mod | gzip -1 >/tmp/go.tar.gz); then
  echo "Couldn't archive Go's caches"
  exit 0
fi
object="$(printf '%s/go.tar.gz' "${SAVE}" | sed 's|/|%2F|g')"
status="$(curl -sS -o /dev/null -w '%{http_code}' -X POST -T /tmp/go.tar.gz \
  -H "Authorization: Bearer ${token}" -H 'Content-Type: application/gzip' \
  "${bucket}/o?uploadType=media&name=${object}" || true)"
if [ "${status}" = 200 ]; then
  echo "Saved ${SAVE}/go.tar.gz: $(du -h /tmp/go.tar.gz | cut -f1)"
else
  echo "Couldn't save ${SAVE}/go.tar.gz (HTTP ${status})"
fi
