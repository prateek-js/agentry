#!/usr/bin/env bash
# Spin throwaway Postgres/MySQL/Mongo/Redis containers, run the storage
# integration suite against all four, then tear them down. Requires Docker.
set -u
cd "$(dirname "$0")/.."

names="agtest-pg agtest-mysql agtest-mongo agtest-redis"
cleanup() { docker rm -f $names >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "→ starting DB containers"
docker run -d --name agtest-pg    -e POSTGRES_PASSWORD=test -p 55432:5432 postgres:16-alpine >/dev/null
docker run -d --name agtest-mysql -e MYSQL_ROOT_PASSWORD=test -e MYSQL_DATABASE=test -p 53306:3306 mysql:8 >/dev/null
docker run -d --name agtest-mongo -p 57017:27017 mongo:7 >/dev/null
docker run -d --name agtest-redis -p 56379:6379 redis:7-alpine >/dev/null

export AUTO_TEST_PG="postgres://postgres:test@127.0.0.1:55432/postgres"
export AUTO_TEST_MYSQL="mysql://root:test@127.0.0.1:53306/test"
export AUTO_TEST_MONGO="mongodb://127.0.0.1:57017/agentrytest"
export AUTO_TEST_REDIS="redis://127.0.0.1:56379"

echo "→ running integration suite (driver retries handle warm-up)"
node test/integration.mjs
