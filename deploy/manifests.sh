#!/bin/sh
# Renders deploy/ into OUT the way Argo CD deploys it, and validates every resource with kubeconform: once as written,
# and once for each Application in delivery's main that deploys octomaton's deploy/, with its kustomize options applied
# as Argo CD 3.5 applies them (util/kustomize/kustomize.go): images (environment variables substituted), then its
# patches appended to kustomization.yaml. Runs in docker.io/alpine/k8s, whose kustomize is Argo CD's (v5.8.1).
#
#   REVISION=<commit> deploy/manifests.sh OUT
#
# DELIVERY=<directory> uses that checkout of arikkfir-org/delivery instead of cloning its main.
set -eu

out="${1:?usage: REVISION=<commit> deploy/manifests.sh OUT}"
revision="${REVISION:?REVISION must be the commit under test}"
octomaton=https://github.com/arikkfir-org/octomaton

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${out}"
failures=0

kustomize build deploy > "${out}/deploy.yaml"
echo "deploy/ as written: ${out}/deploy.yaml"

delivery="${DELIVERY:-}"
if [ -z "${delivery}" ]; then
  delivery="${work}/delivery"
  git clone --quiet --depth=1 https://github.com/arikkfir-org/delivery "${delivery}"
fi

# Every Application in delivery that deploys octomaton's deploy/, one per line as JSON: its name and its kustomize
# options. Only the files that name octomaton's repository are read.
find "${delivery}/apps" "${delivery}/platform" -name '*.yaml' > "${work}/files"
: > "${work}/sources.json"
while read -r file; do
  status=0
  grep -q -F "${octomaton}" "${file}" || status=$?
  case "${status}" in
    0) ;;
    1) continue ;;
    *)
      echo "could not read ${file}" >&2
      failures=$((failures + 1))
      continue
      ;;
  esac
  if ! yq ea -o=json -I=0 '
    select(.kind == "Application") | . as $app
    | ($app.spec.sources // [$app.spec.source])[]
    | select(.repoURL == "'"${octomaton}"'" and .path == "deploy")
    | {"name": $app.metadata.name, "kustomize": (.kustomize // {})}
  ' "${file}" >> "${work}/sources.json"; then
    echo "could not read ${file}" >&2
    failures=$((failures + 1))
  fi
done < "${work}/files"

# What Argo CD substitutes in images.
export ARGOCD_APP_REVISION="${revision}"
ARGOCD_APP_REVISION_SHORT="$(printf '%s' "${revision}" | cut -c1-7)"
ARGOCD_APP_REVISION_SHORT_8="$(printf '%s' "${revision}" | cut -c1-8)"
export ARGOCD_APP_REVISION_SHORT ARGOCD_APP_REVISION_SHORT_8

# render NAME OPTIONS renders a copy of deploy/ with the kustomize options in the file OPTIONS into OUT/NAME.yaml. It
# runs where set -e doesn't apply, so every step returns on failure.
render() {
  name="$1"
  options="$2"
  # Only the options this applies: with any other, Argo CD would render something else.
  others="$(yq '[keys[] | select(. != "images" and . != "patches")] | join(", ")' "${options}")" || return 1
  if [ -n "${others}" ]; then
    echo "${name}: kustomize options this script doesn't apply: ${others}" >&2
    return 1
  fi
  images="$(yq '.images[]? | envsubst(nu)' "${options}")" || return 1
  patches="$(yq '.patches // [] | length' "${options}")" || return 1

  # kustomize skips a patch whose target matches nothing, and Argo CD would deploy without it: a rename on either side.
  i=0
  while [ "${i}" -lt "${patches}" ]; do
    kind="$(yq ".patches[${i}].target.kind // \"\"" "${options}")" || return 1
    target="$(yq ".patches[${i}].target.name // \"\"" "${options}")" || return 1
    if [ -n "${kind}${target}" ]; then
      matches="$(KIND="${kind}" TARGET="${target}" yq '
        select((strenv(KIND) == "" or .kind == strenv(KIND)) and (strenv(TARGET) == "" or .metadata.name == strenv(TARGET)))
        | .kind
      ' "${out}/deploy.yaml" | grep -c .)" || true
      if [ "${matches}" -eq 0 ]; then
        echo "${name}: patch ${i}'s target (kind ${kind:-any}, name ${target:-any}) matches nothing in deploy/" >&2
        return 1
      fi
    fi
    i=$((i + 1))
  done

  cp -R deploy "${work}/${name}" || return 1
  (
    cd "${work}/${name}" || exit 1
    for image in ${images}; do
      kustomize edit set image "${image}" || exit 1
    done
    if [ "${patches}" -gt 0 ]; then
      OPTIONS="${options}" yq -i '.patches += load(env(OPTIONS)).patches' kustomization.yaml || exit 1
    fi
  ) || return 1
  kustomize build "${work}/${name}" > "${out}/${name}.yaml" || return 1
  echo "deploy/ as delivery's Application ${name} deploys it: ${out}/${name}.yaml"
}

sources=0
while read -r source; do
  sources=$((sources + 1))
  name="$(printf '%s' "${source}" | yq -p=json '.name')"
  printf '%s' "${source}" | yq -p=json -o=yaml '.kustomize' > "${work}/${name}.options.yaml"
  if ! render "${name}" "${work}/${name}.options.yaml"; then
    echo "could not render deploy/ as delivery's Application ${name} deploys it" >&2
    failures=$((failures + 1))
  fi
done < "${work}/sources.json"
# The hub runs Octomaton from delivery's main: finding no Application there means this check lost track of it.
if [ "${sources}" -eq 0 ]; then
  echo "no Application in delivery's main deploys ${octomaton}'s deploy/" >&2
  failures=$((failures + 1))
fi

kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
  "${out}"

if [ "${failures}" -gt 0 ]; then
  echo "${failures} failure(s) above" >&2
  exit 1
fi
