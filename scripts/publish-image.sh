#!/usr/bin/env bash
set -euo pipefail

: "${GHCR_TOKEN:?GHCR_TOKEN is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_ACTOR:?GITHUB_ACTOR is required}"
: "${GITHUB_SHA:?GITHUB_SHA is required}"
: "${GITHUB_REF:?GITHUB_REF is required}"

registry_image="ghcr.io/${GITHUB_REPOSITORY,,}"
printf '%s' "$GHCR_TOKEN" | skopeo login ghcr.io --username "$GITHUB_ACTOR" --password-stdin
trap 'skopeo logout ghcr.io >/dev/null' EXIT
skopeo copy docker-archive:result "docker://$registry_image:sha-$GITHUB_SHA"
if [[ "$GITHUB_REF" == refs/tags/* ]]; then
  skopeo copy docker-archive:result "docker://$registry_image:${GITHUB_REF_NAME:?}"
elif [[ "$GITHUB_REF" == refs/heads/main ]]; then
  skopeo copy docker-archive:result "docker://$registry_image:latest"
fi
