# The pull request reviewer's image (arikkfir-org/tooling, reviewer/pipelinerun.yaml): opencode's official image plus the
# tools the model runs while it reviews. .tekton/reviewer-image.yaml publishes it from main as
# me-west1-docker.pkg.dev/arikkfir/images/reviewer, tagged with the commit's short SHA and "main"; tooling runs its
# "main" tag.
FROM ghcr.io/anomalyco/opencode:1.18.33@sha256:ee31dff80f8347b4705317de808b01fce1cfbf35cff560ddc573cda9718be0f7

# Alpine 3.24 packages. opencode runs the model's commands in bash once it is on PATH. GNU coreutils, findutils, grep,
# sed, gawk, diffutils, tar and wget replace busybox's applets, whose options differ. yq-go is mikefarah's yq.
RUN apk add --no-cache \
      bash \
      coreutils \
      curl \
      diffutils \
      file \
      findutils \
      gawk \
      git \
      grep \
      gzip \
      jq \
      less \
      make \
      patch \
      python3 \
      sed \
      tar \
      tree \
      unzip \
      wget \
      xz \
      yq-go
