# learn-claude-code-go

这是一个用于学习并实践 [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code) 的 Go 版本实现。  
项目使用 **CloudWeGo Eino** 框架来实现 Claude Code 风格的 Agent 示例。

## 快速开始

**1. 克隆仓库并配置环境变量**

```bash
git clone https://github.com/shareAI-lab/learn-claude-code-go.git
cd learn-claude-code-go
cp .env.example .env
# 编辑 .env，填入 ANTHROPIC_API_KEY 和 MODEL_ID
```

**2. 一键下载所有依赖**

```bash
# Linux / macOS
bash setup.sh

# Windows (PowerShell)
.\setup.ps1
```

**3. 运行任意示例**

```bash
go run ./agents/s01_agent_loop
```

---

## 项目目标

- 对照 Python 教程版本，逐步实现 Go 版 Agent
- 理解 Agent Loop、Tool Use、工具分发等核心模式
- 在 Go 生态下使用 Eino 完成可运行的工程化实践

## 当前实现

- `s01_agent_loop`：最小可用 Agent Loop
- `s02_tool_use`：工具调用与工具分发（`bash` / `read_file` / `write_file` / `edit_file`）
- `s03_todo_write`：TodoWrite 规划 —— 带状态的 TodoManager + nag reminder 注入
- `s04_subagent`：Subagent 模式 —— 用 task 工具派生子 Agent，子 Agent 独立上下文，只向父 Agent 返回摘要
- `s05_skill_loading`：Skill Loading —— 两层技能注入，系统提示只含名称，按需加载完整技能体
- `s06_context_compact`：Context Compact —— 三层压缩管道（micro/auto/manual），支持无限长会话
- `s07_task_system`：Task System —— 任务持久化为 JSON 文件，支持依赖图（blockedBy/blocks），上下文压缩后任务状态不丢失
- `s08_background_tasks`：Background Tasks —— 用 goroutine 后台执行命令，结果通过通知队列在下次 LLM 调用前注入
- `s09_agent_teams`：Agent Teams —— 持久化命名 Agent，JSONL 文件信箱，多 goroutine 并发协作
- `s10_team_protocols`：Team Protocols —— Shutdown 和 Plan Approval 协议，request_id 关联追踪
- `s11_autonomous_agents`：Autonomous Agents —— Idle 循环、任务轮询、自动认领、身份重注入
- `s12_worktree_task_isolation`：Worktree Task Isolation —— 目录级隔离、事件总线、并行执行

## 项目结构

```text
learn-claude-code-go/
├─ agents/
│  ├─ s01_agent_loop/
│  │  ├─ main.go
│  │  ├─ go.mod
│  │  └─ go.sum
│  ├─ s02_tool_use/
│  │  ├─ main.go
│  │  ├─ go.mod
│  │  └─ go.sum
│  ├─ s03_todo_write/
│  │  ├─ main.go
│  │  ├─ go.mod
│  │  └─ go.sum
│  ├─ s04_subagent/
│  │  ├─ main.go
│  │  ├─ go.mod
│  │  └─ go.sum
│  ├─ s05_skill_loading/
│  │  ├─ main.go
│  │  ├─ go.mod
│  │  └─ go.sum
│  ├─ s06_context_compact/
│  │  ├─ main.go
│  │  ├─ go.mod
│  │  └─ go.sum
│  ├─ s07_task_system/
│  │  ├─ main.go
│  │  ├─ go.mod
│  │  └─ go.sum
│  ├─ s08_background_tasks/
│  │  ├─ main.go
│  │  ├─ go.mod
│  │  └─ go.sum
│  └─ s09_agent_teams/
│     ├─ main.go
│     ├─ go.mod
│     └─ go.sum
│  └─ s10_team_protocols/
│     ├─ main.go
│     ├─ go.mod
│     └─ go.sum
│  └─ s11_autonomous_agents/
│     ├─ main.go
│     ├─ go.mod
│     └─ go.sum
│  └─ s12_worktree_task_isolation/
│     ├─ main.go
│     ├─ go.mod
│     └─ go.sum
├─ skills/
│  ├─ agent-builder/SKILL.md
│  ├─ code-review/SKILL.md
│  ├─ mcp-builder/SKILL.md
│  └─ pdf/SKILL.md
├─ go.work
├─ setup.sh
├─ setup.ps1
├─ .env.example
└─ .env
```

## 环境变量

从 `.env.example` 复制一份到 `.env`：

- `ANTHROPIC_API_KEY`（必填）
- `MODEL_ID`（必填）
- `ANTHROPIC_BASE_URL`（可选，使用兼容 Anthropic 的服务商时配置）

## 运行方式（go.work）

在项目根目录执行：

```bash
# 运行 s01
go run ./agents/s01_agent_loop

# 运行 s02
go run ./agents/s02_tool_use

# 运行 s03
go run ./agents/s03_todo_write

# 运行 s04
go run ./agents/s04_subagent

# 运行 s05
go run ./agents/s05_skill_loading

# 运行 s06
go run ./agents/s06_context_compact

# 运行 s07
go run ./agents/s07_task_system

# 运行 s08
go run ./agents/s08_background_tasks

# 运行 s09
go run ./agents/s09_agent_teams

# 运行 s10
go run ./agents/s10_team_protocols

# 运行 s11
go run ./agents/s11_autonomous_agents

# 运行 s12
go run ./agents/s12_worktree_task_isolation
```

也可以先构建：

```bash
go build -o bin/s01_agent_loop ./agents/s01_agent_loop
go build -o bin/s02_tool_use ./agents/s02_tool_use
go build -o bin/s03_todo_write ./agents/s03_todo_write
go build -o bin/s04_subagent ./agents/s04_subagent
go build -o bin/s05_skill_loading ./agents/s05_skill_loading
go build -o bin/s06_context_compact ./agents/s06_context_compact
go build -o bin/s07_task_system ./agents/s07_task_system
go build -o bin/s08_background_tasks ./agents/s08_background_tasks
go build -o bin/s09_agent_teams ./agents/s09_agent_teams
go build -o bin/s10_team_protocols ./agents/s10_team_protocols
go build -o bin/s11_autonomous_agents ./agents/s11_autonomous_agents
go build -o bin/s12_worktree_task_isolation ./agents/s12_worktree_task_isolation
```

## 说明

- 本项目为学习用途，主要关注 Agent 设计模式与工程组织
- 示例中包含基础安全限制（如危险命令拦截、工作区路径约束）
- 项目已完成 s01~s12 全部实现
