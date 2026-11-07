#!/bin/sh
# Esegue le suite di regressione in un container Python dedicato, sulla stessa rete del backend.
# Uso: docker/run-tests.sh [base_url]   (predefinito: http://opengencut-api:8080)
set -e
BASE="${1:-http://opengencut-api:8080}"
cd "$(dirname "$0")/.."
docker run --rm --network mon_mysql_net \
  -v "$PWD/tests:/tests:ro" -v "$PWD/sdk:/sdk:ro" \
  -e GENCUT_TEST_USER="${GENCUT_TEST_USER:-qa-tester}" \
  -e GENCUT_TEST_PASSWORD="${GENCUT_TEST_PASSWORD:-}" \
  -e GENCUT_ADMIN_PASSWORD="${GENCUT_ADMIN_PASSWORD:-}" \
  -e GENCUT_BASE_URL="$BASE" \
  python:3.12-slim sh -c "for t in /tests/regression_*.py; do echo \"== \$(basename \$t)\"; python3 -I \$t $BASE | tail -1; done"
