#!/usr/bin/env bash
# Brand gate: BeaconX must not carry visible traces of any third-party project
# it was studied from. This check runs in CI on every pull request.
#
# Forbidden words are assembled from fragments so this file does not trip the
# scan it performs.
set -euo pipefail

cd "$(dirname "$0")/.."

a1="ar";          a2="tex"
u1="au";          u2="tumn";        u3="-";  u4="27"
t1="to";          t2="micdd"
s1="scope";       s2="sentry"

words="${a1}${a2}|${u1}${u2}${u3}${u4}|${t1}${t2}|${s1}${s2}"
# Whole-token match only: "cleartext" or "scope" must not trip the gate, while
# a path containing the project name, or the name surrounded by punctuation,
# still must.
pattern="(^|[^[:alnum:]_])(${words})([^[:alnum:]_]|$)"
failed=0

# 1) Tracked and untracked-but-not-ignored text files.
files=$(git ls-files -co --exclude-standard || true)
if [ -n "$files" ]; then
  if hits=$(printf '%s\n' "$files" | xargs grep -IinE "$pattern" 2>/dev/null); then
    echo "brand gate: forbidden identifier found in repository files:"
    echo "$hits"
    failed=1
  fi
fi

# 2) Module dependencies (offline check of the manifests).
if grep -iE "(^|[^[:alnum:]_])(${u1}${u2}${u3}${u4}|${a1}${a2})([^[:alnum:]_]|$)" go.mod go.sum >/dev/null 2>&1; then
  echo "brand gate: forbidden module dependency"
  grep -iE "(^|[^[:alnum:]_])(${u1}${u2}${u3}${u4}|${a1}${a2})([^[:alnum:]_]|$)" go.mod go.sum
  failed=1
fi

# 3) Built binary strings (when a build exists).
if [ -f bin/beaconx ] && command -v strings >/dev/null 2>&1; then
  if strings bin/beaconx | grep -iE "$pattern" >/dev/null; then
    echo "brand gate: forbidden identifier embedded in binary"
    failed=1
  fi
fi

if [ "$failed" -ne 0 ]; then
  echo "brand gate: FAILED"
  exit 1
fi
echo "brand gate: OK"
