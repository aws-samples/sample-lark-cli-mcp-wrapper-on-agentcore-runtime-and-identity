#!/usr/bin/env bash
# Copyright (c) 2026 panlm
# SPDX-License-Identifier: MIT
#
# Detects drift between this package's vendored domain table and upstream
# lark-cli's internal/core/types.go at a given version tag. internal/ packages
# can never be imported cross-module (Go language rule, not a version issue),
# so this is the only way to catch upstream changes without vendoring the
# whole lark-cli tree.
#
# Run this whenever bumping LARK_CLI_VERSION in deploy/agentcore-runtime/Dockerfile.
#
# Usage: ./check_drift.sh v1.0.72
set -euo pipefail

VERSION="${1:?usage: check_drift.sh <lark-cli version tag, e.g. v1.0.72>}"
URL="https://raw.githubusercontent.com/larksuite/cli/${VERSION}/internal/core/types.go"

echo "Fetching upstream internal/core/types.go @ ${VERSION} ..."
upstream=$(curl -L --connect-timeout 10 --max-time 30 -sS "$URL")

if [ -z "$upstream" ]; then
  echo "ERROR: could not fetch $URL (bad version tag, or upstream moved/renamed the file)" >&2
  exit 1
fi

echo "--- upstream domain-relevant lines ---"
echo "$upstream" | grep -E 'Brand[A-Za-z]+ LarkBrand|"https://|type Endpoints|type LarkBrand'

echo ""
echo "--- this package's domain-relevant lines ---"
grep -E 'Brand[A-Za-z]+ Brand =|"https://|type Endpoints|type Brand' "$(dirname "$0")/endpoints.go"

echo ""
echo "Compare the two blocks above by eye. If upstream added/changed a brand,"
echo "domain, or Endpoints field, update endpoints.go to match."
