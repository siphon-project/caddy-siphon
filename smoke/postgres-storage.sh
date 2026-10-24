#!/usr/bin/env bash
# Run-time smoke test for the PostgreSQL storage module, against a built image:
#
#   1. node 1 starts with its state in a throwaway PostgreSQL and issues a certificate for a.test
#      from Caddy's internal CA, which must land in the objects table, with its lock released after;
#   2. the autosaved config holds the {env.*} placeholder, never the password;
#   3. node 2 starts on the same database and serves that certificate without issuing its own: the
#      stored rows are unchanged (same keys, same modification times).
#
# Usage: smoke/postgres-storage.sh [image]   (default caddy-siphon:ci)
set -euo pipefail

image="${1:-caddy-siphon:ci}"
here="$(cd "$(dirname "$0")" && pwd)"
name="caddy-pg-smoke-$$"
password="smoke-password-not-a-secret"
dsn="postgres://caddy:${password}@${name}-db:5432/caddy?sslmode=disable"

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		for container in "${name}-node1" "${name}-node2"; do
			echo "--- logs of ${container}" >&2
			docker logs "$container" >&2 2>&1 || true
		done
	fi
	docker rm -f "${name}-node1" "${name}-node2" "${name}-db" >/dev/null 2>&1 || true
	docker network rm "$name" >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT

# Logs are read into a variable before grep sees them: `docker logs | grep -q` under pipefail fails
# exactly when grep matches early and the writer gets SIGPIPE.
logs_of() {
	docker logs "${name}-$1" 2>&1
}

sql() {
	docker exec "${name}-db" psql -U caddy -d caddy -At -c "$1"
}

wait_for() { # description, seconds, command...
	local description="$1" seconds="$2"
	shift 2
	for _ in $(seq "$seconds"); do
		if "$@"; then
			return 0
		fi
		sleep 1
	done
	echo "FAIL: timed out waiting for ${description}" >&2
	return 1
}

docker network create "$name" >/dev/null
docker run -d --name "${name}-db" --network "$name" \
	-e POSTGRES_USER=caddy -e POSTGRES_PASSWORD="$password" -e POSTGRES_DB=caddy \
	postgres:17-alpine >/dev/null
# Over TCP, not the socket: the entrypoint's init runs a temporary socket-only server and then restarts,
# so a socket check passes during init and node 1 would connect into the restart (SQLSTATE 57P03).
wait_for "PostgreSQL" 60 docker exec "${name}-db" pg_isready -h 127.0.0.1 -U caddy -d caddy -q

start_node() {
	docker run -d --name "${name}-$1" --network "$name" \
		-e CADDY_STORAGE_DSN="$dsn" \
		-v "${here}/postgres-storage.Caddyfile:/etc/caddy/Caddyfile:ro" \
		"$image" >/dev/null
}

certificate="certificates/local/a.test/a.test.crt"
has_certificate() {
	[ "$(sql "SELECT count(*) FROM caddy_objects WHERE key = '${certificate}'" 2>/dev/null)" = "1" ]
}
no_locks() {
	[ "$(sql "SELECT count(*) FROM caddy_locks" 2>/dev/null)" = "0" ]
}

start_node node1
wait_for "node 1 to store the a.test certificate" 60 has_certificate
wait_for "node 1 to release its issuance lock" 30 no_locks
# Positive control for the node 2 check below: the message it looks for is the one issuance logs.
node1_logs="$(logs_of node1)"
grep -q 'certificate obtained successfully' <<<"$node1_logs" ||
	{ echo "FAIL: node 1 issued without logging 'certificate obtained successfully'; the node 2 check would prove nothing" >&2; exit 1; }
echo "node 1 issued a.test into PostgreSQL and released its lock"

autosave="$(docker exec "${name}-node1" cat /config/caddy/autosave.json)"
if grep -qF "$password" <<<"$autosave"; then
	echo "FAIL: the database password is in autosave.json" >&2
	exit 1
fi
grep -qF '"dsn":"{env.CADDY_STORAGE_DSN}"' <<<"$autosave" ||
	{ echo "FAIL: autosave.json lacks the dsn placeholder" >&2; echo "$autosave" >&2; exit 1; }
echo "autosave.json holds the placeholder, not the password"

before="$(sql "SELECT key, modified FROM caddy_objects ORDER BY key")"
[ "$(wc -l <<<"$before")" -ge 3 ] || { echo "FAIL: expected the CA and certificate files, got:" >&2; echo "$before" >&2; exit 1; }

start_node node2
node2_serving() {
	local logs
	logs="$(logs_of node2)"
	grep -q 'serving initial configuration' <<<"$logs"
}
wait_for "node 2 to start" 60 node2_serving
sleep 5 # give node 2 time to (wrongly) issue, if it were going to
after="$(sql "SELECT key, modified FROM caddy_objects ORDER BY key")"
if [ "$before" != "$after" ]; then
	echo "FAIL: node 2 changed the stored state instead of using it" >&2
	diff <(echo "$before") <(echo "$after") >&2 || true
	exit 1
fi
node2_logs="$(logs_of node2)"
if grep -q 'certificate obtained successfully' <<<"$node2_logs"; then
	echo "FAIL: node 2 obtained a certificate of its own" >&2
	exit 1
fi
for node in node1 node2; do
	errors="$(logs_of "$node" | grep -E '"level":"error"' || true)"
	if grep -qi 'storage\|lock' <<<"$errors"; then
		echo "FAIL: ${node} logged a storage or lock error" >&2
		exit 1
	fi
done
echo "node 2 started on the same database and used node 1's certificate ($(wc -l <<<"$after") keys unchanged)"
