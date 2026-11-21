#!/usr/bin/env bash
set -euo pipefail
revision=${1:?Provide the checked release commit SHA}
[[ "$revision" =~ ^[a-f0-9]{40}$ ]] || { echo 'A full commit SHA is required.' >&2; exit 1; }
container=neochat-server
previous=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' "$container")
project=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' "$container")
config_files=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.config_files" }}' "$container")
[[ -n "$previous" && -n "$project" && -n "$config_files" && -f "$previous/.env" ]] || { echo 'Existing Compose deployment and .env could not be located.' >&2; exit 1; }
release=$(mktemp -d /root/neochat-release.XXXXXX)
curl -fsSL --retry 2 --max-time 120 "https://codeload.github.com/skipjust12/neochat/tar.gz/$revision" -o "$release/source.tar.gz"
tar -xzf "$release/source.tar.gz" --strip-components=1 -C "$release"
rm "$release/source.tar.gz"
cp -p "$previous/.env" "$release/.env"
old_compose=(docker compose -p "$project")
compose=(docker compose -p "$project")
IFS=',' read -r -a configs <<< "$config_files"
for config in "${configs[@]}"; do
  [[ -f "$config" ]] || { echo "Missing live Compose config: $config" >&2; exit 1; }
  old_compose+=(-f "$config")
  if [[ "$config" == "$previous/"* ]]; then
    relative=${config#"$previous/"}
    target="$release/$relative"
    if [[ ! -f "$target" ]]; then mkdir -p "$(dirname "$target")"; cp -p "$config" "$target"; fi
    compose+=(-f "$target")
  else
    compose+=(-f "$config")
  fi
done
old_image=$(docker inspect --format '{{.Image}}' "$container")
image_name=$(docker inspect --format '{{.Config.Image}}' "$container")
rollback_tag="neochat-release-rollback:$(date -u +%Y%m%d%H%M%S)"
docker image tag "$old_image" "$rollback_tag"
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'cd %q\n' "$previous"
  printf 'docker image tag %q %q\n' "$rollback_tag" "$image_name"
  printf '%q ' "${old_compose[@]}" up -d --no-deps --no-build --wait --wait-timeout 120 server
  printf '\n'
} > "$release/rollback.sh"
chmod 700 "$release/rollback.sh"
echo "Release directory: $release"
echo "Rollback: bash $release/rollback.sh"
cd "$release"
# Keep the existing Compose project and dependency containers/volumes.
"${compose[@]}" build server
if ! "${compose[@]}" up -d --no-deps --no-build --wait --wait-timeout 120 server; then
  bash "$release/rollback.sh"
  exit 1
fi
domain=$(docker exec "$container" printenv NEOCHAT_DOMAIN || true)
if [[ -n "$domain" ]]; then
  if ! curl -fsS --retry 2 --max-time 30 "https://$domain/" -o "$release/served.html"; then
    echo 'Public HTTPS check failed; rolling back.' >&2
    bash "$release/rollback.sh"
    exit 1
  fi
  if ! grep -qF '/assets/markdown/markdown-it.min.js?v=15.0.3' "$release/served.html" || ! grep -qF 'responseFont' "$release/served.html"; then
    echo 'Public frontend does not match the release; rolling back.' >&2
    bash "$release/rollback.sh"
    exit 1
  fi
fi
"${compose[@]}" ps server
echo "Deployed $revision. Rollback: bash $release/rollback.sh"
