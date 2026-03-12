#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY_CLIENT_DIR="$ROOT_DIR/clients/python"
VENV_DIR="$PY_CLIENT_DIR/.venv-smoke"

if python3 -m venv "$VENV_DIR" >/dev/null 2>&1; then
  # shellcheck disable=SC1091
  source "$VENV_DIR/bin/activate"
  python -m pip install --upgrade pip
  python -m pip install -r "$PY_CLIENT_DIR/requirements.txt"
  python -m unittest discover -s "$PY_CLIENT_DIR/tests" -p "test_*.py" -v
else
  echo "[python-smoke] python3-venv unavailable; running smoke tests with system Python only." >&2
  echo "[python-smoke] To enable isolated install mode, install python3-venv." >&2
  python3 -m unittest discover -s "$PY_CLIENT_DIR/tests" -p "test_*.py" -v
fi
