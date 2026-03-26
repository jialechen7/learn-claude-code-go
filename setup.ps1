# setup.ps1 - Download all module dependencies (Windows PowerShell)
# Usage: .\setup.ps1

$ErrorActionPreference = "Stop"

Write-Host "==> go work sync ..."
go work sync

$modules = @(
    "agents\s01_agent_loop",
    "agents\s02_tool_use",
    "agents\s03_todo_write",
    "agents\s04_subagent"
)

foreach ($mod in $modules) {
    Write-Host "==> go mod tidy: $mod"
    Push-Location $mod
    go mod tidy
    Pop-Location
}

Write-Host ""
Write-Host "[OK] All dependencies downloaded."
Write-Host "     Run: go run ./agents/s01_agent_loop"
