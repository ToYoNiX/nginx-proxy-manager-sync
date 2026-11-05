#!/bin/sh
# Starts two real NPM instances, seeds the master, runs npm-sync once and
# checks that the backup matches (spec 10.4). Needs docker compose and Go.
set -eu
cd "$(dirname "$0")"
project=npmsynctest

cleanup() { docker compose -p "$project" down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
docker compose -p "$project" up -d

echo "waiting for NPM to start..."
for port in 18181 18182; do
  i=0
  until curl -sf "http://127.0.0.1:$port/api/" | grep -q '"status":"OK"'; do
    i=$((i + 1))
    [ "$i" -lt 120 ] || { echo "NPM on port $port did not start"; exit 1; }
    sleep 2
  done
done

go test -tags integration -count=1 -v .
