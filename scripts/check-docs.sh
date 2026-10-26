#!/usr/bin/env bash
# 文档口径一致性门禁(B8):
#   1. README 徽章的 tests 数 == 全仓 "^func Test" 实测数
#   2. AGENTS.md "当前状态"章节头版本 == CHANGELOG 最新版本
# 防的是"README 停在旧版本/旧测试数"这类硬伤(v4.3.0 徽章 vs v4.7.8 实况的事故复盘)。
set -euo pipefail
cd "$(dirname "$0")/.."

latest=$(grep -m1 -oE '\[v[0-9]+\.[0-9]+\.[0-9]+\]' CHANGELOG.md | tr -d '[]')
if [ -z "$latest" ]; then echo "FAIL: CHANGELOG.md 里找不到版本条目"; exit 1; fi

tests=$(grep -rh '^func Test' --include='*.go' . | wc -l | tr -d ' ')
go_files=$(find . -name '*.go' -not -path './.git/*' | wc -l | tr -d ' ')
fail=0

check() { # $1=描述 $2=条件(0=通过)
  if [ "$2" -eq 0 ]; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi
}

for f in README.md README.zh-CN.md; do
  grep -qF "tests-$tests" "$f"; check "$f tests 徽章 == 实测 $tests" $?
done

hdr=$(grep -cF "(v${latest#v})" AGENTS.md || true)
[ "$hdr" -ge 1 ]; check "AGENTS.md 当前状态章节头含版本 ${latest#v}" $?

# 提示性(不挡门):AGENTS 指标表 Go 文件数
if grep -qF "| Go 文件 | $go_files |" AGENTS.md; then
  echo "ok   AGENTS.md Go 文件数 == 实测 $go_files"
else
  echo "warn AGENTS.md 指标表 Go 文件数 != 实测 $go_files(口径漂移,顺手更新)"
fi

exit $fail
