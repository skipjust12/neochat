#!/usr/bin/env bash
set -euo pipefail
patch_file=$(realpath "${1:-/root/neochat-state-timing.patch}")
container=neochat-server
repo=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' "$container")
project=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' "$container")
config_files=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.config_files" }}' "$container")
if [[ -z "$repo" || -z "$project" || -z "$config_files" || ! -d "$repo" ]]; then
  echo 'Cannot locate the existing Compose deployment. No changes made.' >&2
  exit 1
fi
cd "$repo"
compose=(docker compose -p "$project")
IFS=',' read -r -a files <<< "$config_files"
for file in "${files[@]}"; do
  [[ -f "$file" ]] || { echo "Missing Compose file: $file" >&2; exit 1; }
  compose+=(-f "$file")
done
git apply --check "$patch_file"
backup=$(mktemp -d /root/neochat-state-timing-backup.XXXXXX)
cp -p server/frontend.html "$backup/frontend.html"
old_image=$(docker inspect --format '{{.Image}}' "$container")
image_name=$(docker inspect --format '{{.Config.Image}}' "$container")
rollback_tag="neochat-state-timing-rollback:$(date -u +%Y%m%d%H%M%S)"
docker image tag "$old_image" "$rollback_tag"
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'cd %q\n' "$repo"
  printf 'cp -p %q server/frontend.html\n' "$backup/frontend.html"
  printf 'docker image tag %q %q\n' "$rollback_tag" "$image_name"
  printf '%q ' "${compose[@]}" up -d --no-deps --no-build --wait --wait-timeout 120 server
  printf '\n'
} > "$backup/rollback.sh"
chmod 700 "$backup/rollback.sh"
echo "Rollback command: bash $backup/rollback.sh"
git apply "$patch_file"
if ! "${compose[@]}" build server; then
  cp -p "$backup/frontend.html" server/frontend.html
  echo 'Build failed; source restored. Running container unchanged.' >&2
  exit 1
fi
if ! "${compose[@]}" up -d --no-deps --no-build --wait --wait-timeout 120 server; then
  echo 'Update failed; rolling back.' >&2
  bash "$backup/rollback.sh"
  exit 1
fi
docker exec "$container" wget -q -O- http://127.0.0.1:8080/health
# Confirm the running binary serves the new frontend, without any API key.
docker exec "$container" wget -q -O- http://127.0.0.1:8080/ > "$backup/served.html"
if ! grep -q '1200-(performance.now()-actionShownAt)' "$backup/served.html"; then
  echo 'Updated frontend not found; rolling back.' >&2
  bash "$backup/rollback.sh"
  exit 1
fi
"${compose[@]}" ps server
echo "Updated successfully. Rollback: bash $backup/rollback.sh"
