#!/usr/bin/env bash
# setup.sh - 一键下载所有模块依赖
# 用法: bash setup.sh

set -e

echo "==> 同步 go.work.sum ..."
go work sync

modules=(
  ./agents/s01_agent_loop
  ./agents/s02_tool_use
  ./agents/s03_todo_write
  ./agents/s04_subagent
  ./agents/s05_skill_loading
  ./agents/s06_context_compact
  ./agents/s07_task_system
  ./agents/s08_background_tasks
  ./agents/s09_agent_teams
)

for mod in "${modules[@]}"; do
  echo "==> go mod tidy: $mod"
  (cd "$mod" && go mod tidy)
done

echo ""
echo "✓ 所有依赖下载完成。"
echo "  运行示例: go run ./agents/s01_agent_loop"
