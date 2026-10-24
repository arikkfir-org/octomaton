#!/bin/sh
# Fetches ko's release binary into /tools, at the Makefile's version, checked against its SHA-256: built with go run,
# its modules and packages would fill Go's caches, which keep only what Octomaton builds with
# (docs/octomaton/designs/ci-caches.md). Runs in curlimages/curl.
set -eu
version=0.19.1
sha256=635ac6ea3fd376c935fee597fbb29ab2c2449f49ef1655085fe3aa9c25fed7a5
curl -fsSL -o /tmp/ko.tar.gz \
  "https://github.com/ko-build/ko/releases/download/v${version}/ko_${version}_Linux_x86_64.tar.gz"
echo "${sha256}  /tmp/ko.tar.gz" | sha256sum -c -
tar -xzf /tmp/ko.tar.gz -C /tools ko
