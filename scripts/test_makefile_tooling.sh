#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/sluice-tooling.XXXXXX")"
trap 'rm -rf "$fixture"' EXIT

fake_gopath="$fixture/global-gopath"
workspace="$fixture/workspace"
log="$fixture/invocations.log"
mkdir -p "$fake_gopath/bin" "$workspace"

cat >"$fake_gopath/bin/golangci-lint" <<'EOF'
#!/usr/bin/env bash
echo stale-global-linter >>"$SLUICE_TOOLING_TEST_LOG"
exit 79
EOF
chmod +x "$fake_gopath/bin/golangci-lint"

cat >"$fixture/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == "env" && "${2:-}" == "GOPATH" ]]; then
  printf '%s\n' "$SLUICE_TOOLING_TEST_GOPATH"
  exit 0
fi

if [[ "${1:-}" == "install" ]]; then
  mkdir -p "$GOBIN"
  cat >"$GOBIN/golangci-lint" <<'LINTER'
#!/usr/bin/env bash
echo repo-local-linter >>"$SLUICE_TOOLING_TEST_LOG"
LINTER
  chmod +x "$GOBIN/golangci-lint"
  exit 0
fi

printf 'unexpected fake go invocation: %s\n' "$*" >&2
exit 80
EOF
chmod +x "$fixture/go"

SLUICE_TOOLING_TEST_GOPATH="$fake_gopath" \
SLUICE_TOOLING_TEST_LOG="$log" \
  make --no-print-directory -C "$workspace" -f "$repo_root/Makefile" \
    lint GO="$fixture/go"

grep -qx 'repo-local-linter' "$log"
if grep -q 'stale-global-linter' "$log"; then
  echo "lint target used a stale GOPATH binary" >&2
  exit 1
fi

echo "makefile tooling isolation: ok"
