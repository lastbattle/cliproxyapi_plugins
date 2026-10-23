#!/usr/bin/env bash
set -euo pipefail
: "${MIA_RECIPE_DIR:?Set MIA_RECIPE_DIR to the recipe checkout}"
cd "$MIA_RECIPE_DIR"
# start.sh already coordinates both ranks and waits for model readiness.
./start.sh restart
exec python3 -u "$(dirname "$(realpath "$0")")/watchdog.py"
