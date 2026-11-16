#!/usr/bin/env bash
# Fork-only script (not present upstream): the `migrations-roundtrip` check of
# .github/workflows/ci-fork.yml. See docs/fork-ci.md.
#
# Proves, with the real migration runner and `pg_dump --schema-only`, that
#   1. upgrade: a database migrated by the base tree, then by the head tree,
#      has exactly the schema of a fresh head database;
#   2. round trip: rolling back every fork migration leaves exactly the
#      schema of a head database that never applied them, and applying them
#      again restores the fresh head schema.
#
# Fork migrations are the versions numbered FORK_MIGRATION_FLOOR and above.
# Upstream ships forward-only migrations whose down files raise on purpose, so
# new upstream versions are covered by the upgrade comparison only. All fork
# migrations take part in the round trip, not only new ones: on an empty
# database it costs seconds, and a later change can break an older down file.
#
# usage: fork-migrations-roundtrip.sh --base <repo> --head <repo> --out <dir>
#
# Environment:
#   DB_URL_PREFIX  connection URL without the database name
#                  (default postgres://multica:multica@localhost:5432/)
#   DB_URL_QUERY   appended after the database name (default ?sslmode=disable)
#   PG_CLIENT      command prefix for psql/pg_dump, e.g. `docker exec -i <id>`,
#                  so the client matches the server's major version (empty =
#                  local binaries, configured through the usual PG* variables)
#   PGUSER         role for psql/pg_dump (default multica)
set -euo pipefail

FORK_MIGRATION_FLOOR=9000

usage() {
  echo "usage: $0 --base <repo> --head <repo> --out <dir>" >&2
}

base_repo=
head_repo=
out_dir=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --base) base_repo=${2:-}; shift 2 ;;
    --head) head_repo=${2:-}; shift 2 ;;
    --out) out_dir=${2:-}; shift 2 ;;
    *) usage; exit 2 ;;
  esac
done
if [ -z "$base_repo" ] || [ -z "$head_repo" ] || [ -z "$out_dir" ]; then
  usage
  exit 2
fi

DB_URL_PREFIX=${DB_URL_PREFIX:-postgres://multica:multica@localhost:5432/}
DB_URL_QUERY=${DB_URL_QUERY:-?sslmode=disable}
PGUSER=${PGUSER:-multica}
read -r -a pg_client <<<"${PG_CLIENT:-}"

mkdir -p "$out_dir"
out_dir=$(CDPATH="" cd -- "$out_dir" && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fork-migrations-roundtrip.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT

base_migrations="$base_repo/server/migrations"
head_migrations="$head_repo/server/migrations"

versions() {
  find "$1" -maxdepth 1 -name '*.up.sql' -exec basename {} .up.sql \; | LC_ALL=C sort
}

is_fork_version() {
  local number=${1%%_*}
  [[ "$number" =~ ^[0-9]+$ ]] && [ "$((10#$number))" -ge "$FORK_MIGRATION_FLOOR" ]
}

psql_admin() {
  ${pg_client[@]+"${pg_client[@]}"} psql -X -q -v ON_ERROR_STOP=1 -U "$PGUSER" -d postgres "$@"
}

# Session-specific lines (pg_dump's per-run \restrict key and version banner)
# would make every comparison differ, so they are dropped before comparing.
dump_schema() {
  ${pg_client[@]+"${pg_client[@]}"} pg_dump --schema-only --no-owner --no-privileges -U "$PGUSER" -d "$1" \
    | grep -vE '^\\(restrict|unrestrict) |^-- Dumped (from|by) ' >"$out_dir/$2.sql"
}

create_db() {
  psql_admin -c "DROP DATABASE IF EXISTS $1 WITH (FORCE)" -c "CREATE DATABASE $1"
}

# The runner reads ./migrations first (server/internal/migrations.ResolveDir),
# so running a binary from a directory holding a chosen file set applies
# exactly that set, with the same hooks and conditions as production.
migrate() {
  local bin=$1 dir=$2 db=$3 direction=$4
  (cd "$dir" && DATABASE_URL="${DB_URL_PREFIX}${db}${DB_URL_QUERY}" "$bin" "$direction")
}

failures=0
compare() {
  local label=$1 expected=$2 actual=$3
  if diff -u "$out_dir/$expected.sql" "$out_dir/$actual.sql" >"$out_dir/$label.diff"; then
    echo "PASS  $label ($expected.sql == $actual.sql)"
    rm -f "$out_dir/$label.diff"
  else
    echo "FAIL  $label: schemas differ, see $label.diff"
    head -n 80 "$out_dir/$label.diff"
    failures=$((failures + 1))
  fi
}

echo "::group::Build migration runners"
(cd "$base_repo/server" && go build -o "$work_dir/migrate-base" ./cmd/migrate)
(cd "$head_repo/server" && go build -o "$work_dir/migrate-head" ./cmd/migrate)
echo "::endgroup::"

versions "$base_migrations" >"$work_dir/base-versions"
versions "$head_migrations" >"$work_dir/head-versions"
LC_ALL=C comm -13 "$work_dir/base-versions" "$work_dir/head-versions" >"$out_dir/new-versions.txt"

: >"$out_dir/changed-migrations.txt"
while IFS= read -r version; do
  for direction in up down; do
    file="$version.$direction.sql"
    if [ -f "$base_migrations/$file" ] && ! cmp -s "$base_migrations/$file" "$head_migrations/$file"; then
      echo "$file" >>"$out_dir/changed-migrations.txt"
    fi
  done
done < <(LC_ALL=C comm -12 "$work_dir/base-versions" "$work_dir/head-versions")

: >"$out_dir/roundtrip-versions.txt"
while IFS= read -r version; do
  if is_fork_version "$version"; then
    echo "$version" >>"$out_dir/roundtrip-versions.txt"
  fi
done <"$work_dir/head-versions"

echo "new migrations:       $(paste -sd' ' "$out_dir/new-versions.txt")"
echo "changed migrations:   $(paste -sd' ' "$out_dir/changed-migrations.txt")"
echo "round trip:           $(paste -sd' ' "$out_dir/roundtrip-versions.txt")"

echo "::group::Fresh head database"
create_db rt_fresh
migrate "$work_dir/migrate-head" "$head_repo/server" rt_fresh up
dump_schema rt_fresh fresh
echo "::endgroup::"

echo "::group::Base database upgraded by head"
create_db rt_upgrade
migrate "$work_dir/migrate-base" "$base_repo/server" rt_upgrade up
migrate "$work_dir/migrate-head" "$head_repo/server" rt_upgrade up
dump_schema rt_upgrade upgrade
echo "::endgroup::"

compare upgrade-matches-fresh fresh upgrade

if [ -s "$out_dir/roundtrip-versions.txt" ]; then
  mkdir -p "$work_dir/without/migrations" "$work_dir/rollback/migrations"
  for file in "$head_migrations"/*.up.sql; do
    if ! grep -qxF "$(basename "$file" .up.sql)" "$out_dir/roundtrip-versions.txt"; then
      cp "$file" "$work_dir/without/migrations/"
    fi
  done
  while IFS= read -r version; do
    if [ ! -f "$head_migrations/$version.down.sql" ]; then
      echo "FAIL  $version has no down migration"
      failures=$((failures + 1))
      continue
    fi
    cp "$head_migrations/$version.down.sql" "$work_dir/rollback/migrations/"
  done <"$out_dir/roundtrip-versions.txt"

  echo "::group::Head database without the round-trip migrations"
  create_db rt_without
  migrate "$work_dir/migrate-head" "$work_dir/without" rt_without up
  dump_schema rt_without without
  echo "::endgroup::"

  echo "::group::Round trip: down, then up again"
  migrate "$work_dir/migrate-head" "$work_dir/rollback" rt_fresh down
  dump_schema rt_fresh down
  migrate "$work_dir/migrate-head" "$head_repo/server" rt_fresh up
  dump_schema rt_fresh reup
  echo "::endgroup::"

  compare down-matches-without without down
  compare reup-matches-fresh fresh reup
else
  echo "SKIP  round trip: no fork migrations"
fi

if [ "$failures" -gt 0 ]; then
  echo "$failures comparison(s) failed"
  exit 1
fi
echo "all comparisons passed"
