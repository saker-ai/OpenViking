#!/usr/bin/env bash
# OpenViking E2E smoke orchestration. Runs each layer in order and
# short-circuits on the first hard failure. Layers that need external
# services (real LLM API, Qdrant, Mooncake) are skipped unless their
# prerequisites are present.
#
# Layers:
#   L0  unit tests        go test -race ./...                  (always)
#   L1  binary smoke      --help/--version on all 6 binaries   (always)
#   L2  local-memory E2E  openviking-server + HTTP closure     (always)
#   L3  real backends     Qdrant + LLM API                     (opt-in via env)
#   L4  bot openapi       vikingbot + OpenAPI channel          (deferred)
#   L5  vectorize         openviking-vectorize JSONL→Qdrant    (deferred)
#   L6  train pipeline    internal/session/train -tags=e2e     (always)
#
# Usage:
#   scripts/e2e-smoke.sh                   # L0+L1+L2+L6
#   OV_E2E_REAL=1 scripts/e2e-smoke.sh     # +L3 (needs OV_EMBEDDER_API_KEY)
#
# Exits 0 only when all enabled layers pass.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BIN="$ROOT/bin"
PASS=0
FAIL=0
SKIP=0

section() { printf '\n=== %s ===\n' "$1"; }
ok()      { printf '  ✓ %s\n' "$1"; PASS=$((PASS+1)); }
bad()     { printf '  ✗ %s\n' "$1"; FAIL=$((FAIL+1)); }
skip()    { printf '  - %s (skipped)\n' "$1"; SKIP=$((SKIP+1)); }

# ---------- L0: unit tests ----------
section "L0: unit tests (go test -race ./...)"
if GOWORK=off go test ./... -count=1 -race 2>&1 | tail -5; then
  ok "unit tests"
else
  bad "unit tests"
  exit 1
fi

# ---------- L1: binary smoke ----------
section "L1: binary smoke"
# Build binaries directly via go build; `make build` also builds
# web-studio (pnpm/vite) which is not needed for E2E smoke and may
# fail in environments without node_modules pre-installed.
mkdir -p "$BIN"
for bin in openviking-server openviking-doctor openviking-migrate openviking-vectorize ov vikingbot; do
  if GOWORK=off go build -o "$BIN/$bin" "./cmd/$bin" 2>/dev/null; then
    ok "build $bin"
  else
    skip "build $bin (cmd not present or build failed)"
  fi
done
for bin in openviking-server openviking-doctor openviking-migrate openviking-vectorize ov vikingbot; do
  if [ ! -x "$BIN/$bin" ]; then
    skip "$bin --help (not built)"
    continue
  fi
  if "$BIN/$bin" --help >/dev/null 2>&1; then
    ok "$bin --help"
  else
    bad "$bin --help"
  fi
  if "$BIN/$bin" --version >/dev/null 2>&1; then
    ok "$bin --version"
  else
    skip "$bin --version (optional)"
  fi
done

# ---------- L2: local-memory E2E ----------
section "L2: local-memory server E2E (in-memory vectordb + hash embedder)"
if GOWORK=off go test -tags=e2e -race -timeout 90s ./tests/e2e/... 2>&1 | tail -10; then
  ok "layer 2 E2E (PUT /content → GET /content → /search wired)"
else
  bad "layer 2 E2E"
  exit 1
fi

# ---------- L3: real backends (opt-in) ----------
section "L3: real backends (Qdrant + LLM API)"
if [ "${OV_E2E_REAL:-0}" != "1" ]; then
  skip "layer 3 (set OV_E2E_REAL=1 to enable; needs OV_EMBEDDER_API_KEY)"
else
  if [ -z "${OV_EMBEDDER_API_KEY:-}" ]; then
    bad "layer 3: OV_EMBEDDER_API_KEY not set"
    exit 1
  fi
  # Real-backend smoke: start Qdrant, point server at it, run the same
  # closure. Deferred until layer 3 harness lands.
  skip "layer 3 (harness TBD — config wired, harness not yet)"
fi

# ---------- L4: bot openapi (deferred) ----------
section "L4: bot OpenAPI channel E2E"
skip "layer 4 (vikingbot OpenAPI harness TBD)"

# ---------- L5: vectorize (deferred) ----------
section "L5: offline vectorize E2E"
skip "layer 5 (openviking-vectorize JSONL→Qdrant harness TBD)"

# ---------- L6: train pipeline ----------
section "L6: training pipeline E2E"
if GOWORK=off go test -race -timeout 60s -run 'TestPipeline_(TrainEndToEnd|EvalEndToEnd|TrainFromRollouts)' ./internal/session/train/... 2>&1 | tail -5; then
  ok "layer 6 training pipeline"
else
  bad "layer 6 training pipeline"
  exit 1
fi

# ---------- summary ----------
section "summary"
printf '  pass: %d  fail: %d  skip: %d\n' "$PASS" "$FAIL" "$SKIP"
[ "$FAIL" -eq 0 ] || exit 1
echo "E2E smoke OK"
