#!/bin/sh
# Restores Go's caches into /go-cache (docs/octomaton/designs/ci-caches.md): the first archive of RESTORE's
# space-separated names (main, pull-requests/<number>) that exists in bucket arikkfir-octomaton-ci-cache, streamed in.
# Without one, the run downloads and compiles everything. Go marks a build-cache entry it uses by its modification
# time, at most once an hour: dated two hours back here, each entry the run uses shows it, and go-cache-save.sh keeps
# only those. Runs in curlimages/curl, as the step's Workload Identity.
set -eu
token="$(curl -fsS -H 'Metadata-Flavor: Google' \
  http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token |
  sed -n 's/.*"access_token" *: *"\([^"]*\)".*/\1/p')"
bucket=https://storage.googleapis.com/storage/v1/b/arikkfir-octomaton-ci-cache
for cache in ${RESTORE}; do
  object="$(printf '%s/go.tar.gz' "${cache}" | sed 's|/|%2F|g')"
  # shellcheck disable=SC3040 # busybox's sh, curlimages/curl's, has pipefail
  if (set -o pipefail; curl -sf -w '%{stderr}%{http_code}' -H "Authorization: Bearer ${token}" \
    "${bucket}/o/${object}?alt=media" 2>/tmp/status | tar -xzf - -C /go-cache 2>/tmp/tar); then
    echo "Restored ${cache}/go.tar.gz"
    break
  fi
  rm -rf /go-cache/build /go-cache/mod
  case "$(cat /tmp/status)" in
    404) echo "No ${cache}/go.tar.gz yet" ;;
    200) echo "${cache}/go.tar.gz didn't unpack: $(cat /tmp/tar)" ;;
    *) echo "${cache}/go.tar.gz unreadable (HTTP $(cat /tmp/status))" ;;
  esac
done
restored="$(( $(date +%s) - 7200 ))"
if [ -d /go-cache/build ]; then
  find /go-cache/build -path '/go-cache/build/??/*' -type f -exec touch -d "@${restored}" {} + ||
    echo "Couldn't date /go-cache/build: save may drop entries this run uses"
fi
touch /go-cache/.restored
