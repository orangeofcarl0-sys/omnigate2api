#!/usr/bin/env bash
# 启用仓库自带的 git hooks（脱敏守卫）。幂等，可反复执行。
set -e
cd "$(dirname "$0")/.."
chmod +x tools/hooks/pre-commit 2>/dev/null || true
git config core.hooksPath tools/hooks
echo "已启用：core.hooksPath = $(git config core.hooksPath)"
echo "验证：tools/hooks/pre-commit（当前暂存区）"
