#!/usr/bin/env bash
set -euo pipefail
# Pass the pinned raw GitHub scripts URL printed with the release command.
base_url=${1:?Provide the pinned raw GitHub scripts URL}
container=neochat-server
repo=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' "$container")
project=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' "$container")
config_files=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.config_files" }}' "$container")
[[ -n "$repo" && -n "$project" && -n "$config_files" && -d "$repo" ]] || { echo 'Existing Compose deployment not found.' >&2; exit 1; }
cd "$repo"
compose=(docker compose -p "$project")
IFS=',' read -r -a configs <<< "$config_files"
for config in "${configs[@]}"; do
  [[ -f "$config" ]] || { echo "Missing Compose config: $config" >&2; exit 1; }
  compose+=(-f "$config")
done
backup=$(mktemp -d /root/neochat-inline-images-backup.XXXXXX)
patch_file=
for name in inline-images-update.patch inline-images-after-greeting.patch; do
  curl -fsSL "$base_url/$name" -o "$backup/$name"
  if git apply --check "$backup/$name" 2> "$backup/check.log"; then patch_file="$backup/$name"; break; fi
done
[[ -n "$patch_file" ]] || { echo "Source differs from the expected versions. Nothing changed. Details: $backup/check.log" >&2; cat "$backup/check.log" >&2; exit 1; }
# List exactly the paths touched by the patch; no secrets or databases.
mapfile -t paths < <(sed -n 's|^+++ b/||p' "$patch_file")
existing=()
new=()
for path in "${paths[@]}"; do
  if [[ -f "$path" ]]; then existing+=("$path"); else new+=("$path"); fi
done
tar -cf "$backup/source.tar" -- "${existing[@]}"
old_image=$(docker inspect --format '{{.Image}}' "$container")
image_name=$(docker inspect --format '{{.Config.Image}}' "$container")
rollback_tag="neochat-inline-images-rollback:$(date -u +%Y%m%d%H%M%S)"
docker image tag "$old_image" "$rollback_tag"
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'cd %q\n' "$repo"
  printf 'tar -xf %q\n' "$backup/source.tar"
  if ((${#new[@]})); then printf 'rm -f -- '; printf '%q ' "${new[@]}"; printf '\n'; fi
} > "$backup/restore-source.sh"
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'bash %q\n' "$backup/restore-source.sh"
  printf 'cd %q\n' "$repo"
  printf 'docker image tag %q %q\n' "$rollback_tag" "$image_name"
  printf '%q ' "${compose[@]}" up -d --no-deps --no-build --wait --wait-timeout 120 server
  printf '\n'
} > "$backup/rollback.sh"
chmod 700 "$backup/restore-source.sh" "$backup/rollback.sh"
echo "Rollback: bash $backup/rollback.sh"
git apply "$patch_file"
if ! "${compose[@]}" build server; then
  bash "$backup/restore-source.sh"
  echo 'Build failed; sources restored. The running container is unchanged.' >&2
  exit 1
fi
if ! "${compose[@]}" up -d --no-deps --no-build --wait --wait-timeout 120 server; then
  bash "$backup/rollback.sh"
  exit 1
fi
domain=$(docker exec "$container" printenv NEOCHAT_DOMAIN || true)
if [[ -n "$domain" ]]; then
  if curl -fsS --max-time 30 "https://$domain/" -o "$backup/served.html"; then
    if ! grep -qF 'function parseInlineImage(' "$backup/served.html"; then
      echo 'Public frontend does not contain the update; rolling back.' >&2
      bash "$backup/rollback.sh"
      exit 1
    fi
  else
    echo 'Container is healthy; public HTTPS check failed. Check the site manually.' >&2
    exit 1
  fi
fi
"${compose[@]}" ps server
echo "Updated successfully. Rollback: bash $backup/rollback.sh"
