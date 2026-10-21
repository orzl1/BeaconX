#!/usr/bin/env bash
# 品牌门禁：BeaconX 不得残留任何被借鉴的第三方项目的可见痕迹。
# 该检查在 CI 中针对每个拉取请求运行。
#
# 禁用词由分片拼接而成，避免本文件触发自身执行的扫描。
set -euo pipefail

cd "$(dirname "$0")/.."

a1="ar";          a2="tex"
u1="au";          u2="tumn";        u3="-";  u4="27"
t1="to";          t2="micdd"
s1="scope";       s2="sentry"

words="${a1}${a2}|${u1}${u2}${u3}${u4}|${t1}${t2}|${s1}${s2}"
# 仅按整词匹配："cleartext" 或 "scope" 不得命中门禁，
# 而包含项目名的路径、或被标点包裹的项目名，仍然必须命中。
pattern="(^|[^[:alnum:]_])(${words})([^[:alnum:]_]|$)"
failed=0

# 1) 已跟踪的文本文件，以及未跟踪但未被忽略的文本文件。
files=$(git ls-files -co --exclude-standard || true)
if [ -n "$files" ]; then
  if hits=$(printf '%s\n' "$files" | xargs grep -IinE "$pattern" 2>/dev/null); then
    echo "品牌门禁：在仓库文件中发现禁用标识："
    echo "$hits"
    failed=1
  fi
fi

# 2) 模块依赖（对清单文件做离线检查）。
if grep -iE "(^|[^[:alnum:]_])(${u1}${u2}${u3}${u4}|${a1}${a2})([^[:alnum:]_]|$)" go.mod go.sum >/dev/null 2>&1; then
  echo "品牌门禁：发现禁用的模块依赖"
  grep -iE "(^|[^[:alnum:]_])(${u1}${u2}${u3}${u4}|${a1}${a2})([^[:alnum:]_]|$)" go.mod go.sum
  failed=1
fi

# 3) 已构建二进制中的字符串（存在构建产物时）。
if [ -f bin/beaconx ] && command -v strings >/dev/null 2>&1; then
  if strings bin/beaconx | grep -iE "$pattern" >/dev/null; then
    echo "品牌门禁：二进制中内嵌了禁用标识"
    failed=1
  fi
fi

if [ "$failed" -ne 0 ]; then
  echo "品牌门禁：未通过"
  exit 1
fi
echo "品牌门禁：通过"
