package main

// Harness: persistent tasks -- goals that outlive any single conversation.
// s07_task_system - Tasks
//
// Tasks persist as JSON files in .tasks/ so they survive context compression.
// Each task has a dependency graph (blockedBy/blocks).
//
//	.tasks/
//	  task_1.json  {"id":1, "subject":"...", "status":"completed", ...}
//	  task_2.json  {"id":2, "blockedBy":[1], "status":"pending", ...}
//	  task_3.json  {"id":3, "blockedBy":[2], "blocks":[], ...}
//
//	Dependency resolution:
//	+----------+     +----------+     +----------+
//	| task 1   | --> | task 2   | --> | task 3   |
//	| complete |     | blocked  |     | blocked  |
//	+----------+     +----------+     +----------+
//	     |
//	     +--- completing task 1 removes it from task 2's blockedBy
//
// Key insight: "State that survives compression -- because it's outside the conversation."

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/joho/godotenv"
)

const (
	maxToolOutput = 50000
	toolTimeout   = 120 * time.Second
	tasksDir      = ".tasks"
)

var (
	dangerousPatterns = []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	workDir           string
)

// -- TaskManager: CRUD with dependency graph, persisted as JSON files --

type Task struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"` // pending | in_progress | completed
	BlockedBy   []int  `json:"blockedBy"`
	Blocks      []int  `json:"blocks"`
	Owner       string `json:"owner"`
}

type TaskManager struct {
	dir    string
	nextID int
}

func NewTaskManager(dir string) (*TaskManager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tm := &TaskManager{dir: dir}
	tm.nextID = tm.maxID() + 1
	return tm, nil
}

func (tm *TaskManager) maxID() int {
	entries, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	max := 0
	for _, e := range entries {
		base := filepath.Base(e)
		parts := strings.Split(strings.TrimSuffix(base, ".json"), "_")
		if len(parts) == 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n > max {
				max = n
			}
		}
	}
	return max
}

func (tm *TaskManager) taskPath(id int) string {
	return filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", id))
}

func (tm *TaskManager) load(id int) (*Task, error) {
	b, err := os.ReadFile(tm.taskPath(id))
	if err != nil {
		return nil, fmt.Errorf("task %d not found", id)
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (tm *TaskManager) save(t *Task) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tm.taskPath(t.ID), b, 0o644)
}

func (tm *TaskManager) Create(subject, description string) (string, error) {
	t := &Task{
		ID:          tm.nextID,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		BlockedBy:   []int{},
		Blocks:      []int{},
	}
	if err := tm.save(t); err != nil {
		return "", err
	}
	tm.nextID++
	b, _ := json.MarshalIndent(t, "", "  ")
	return string(b), nil
}

func (tm *TaskManager) Get(id int) (string, error) {
	t, err := tm.load(id)
	if err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(t, "", "  ")
	return string(b), nil
}

func (tm *TaskManager) Update(id int, status string, addBlockedBy, addBlocks []int) (string, error) {
	t, err := tm.load(id)
	if err != nil {
		return "", err
	}
	if status != "" {
		if status != "pending" && status != "in_progress" && status != "completed" {
			return "", fmt.Errorf("invalid status: %s", status)
		}
		t.Status = status
		if status == "completed" {
			tm.clearDependency(id)
		}
	}
	if len(addBlockedBy) > 0 {
		t.BlockedBy = uniqueInts(append(t.BlockedBy, addBlockedBy...))
	}
	if len(addBlocks) > 0 {
		t.Blocks = uniqueInts(append(t.Blocks, addBlocks...))
		// Bidirectional: update blocked tasks' blockedBy
		for _, blockedID := range addBlocks {
			if blocked, err := tm.load(blockedID); err == nil {
				blocked.BlockedBy = uniqueInts(append(blocked.BlockedBy, id))
				_ = tm.save(blocked)
			}
		}
	}
	if err := tm.save(t); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(t, "", "  ")
	return string(b), nil
}

func (tm *TaskManager) clearDependency(completedID int) {
	entries, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		var t Task
		if err := json.Unmarshal(b, &t); err != nil {
			continue
		}
		updated := make([]int, 0)
		for _, bid := range t.BlockedBy {
			if bid != completedID {
				updated = append(updated, bid)
			}
		}
		if len(updated) != len(t.BlockedBy) {
			t.BlockedBy = updated
			_ = tm.save(&t)
		}
	}
}

func (tm *TaskManager) ListAll() string {
	entries, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	sort.Strings(entries)
	if len(entries) == 0 {
		return "No tasks."
	}
	var lines []string
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		var t Task
		if err := json.Unmarshal(b, &t); err != nil {
			continue
		}
		marker := map[string]string{
			"pending":     "[ ]",
			"in_progress": "[>]",
			"completed":   "[x]",
		}[t.Status]
		if marker == "" {
			marker = "[?]"
		}
		line := fmt.Sprintf("%s #%d: %s", marker, t.ID, t.Subject)
		if len(t.BlockedBy) > 0 {
			line += fmt.Sprintf(" (blocked by: %v)", t.BlockedBy)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func uniqueInts(s []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func main() {
	if err := godotenv.Overload(); err != nil && !os.IsNotExist(err) {
		fmt.Printf("load .env failed: %v\n", err)
		return
	}
	ctx := context.Background()
	wd, err := os.Getwd()
	if err != nil {
		fmt.Printf("get cwd failed: %v\n", err)
		return
	}
	workDir = wd

	tm, err := NewTaskManager(filepath.Join(workDir, tasksDir))
	if err != nil {
		fmt.Printf("init task manager failed: %v\n", err)
		return
	}

	cm, err := newClaudeModel(ctx)
	if err != nil {
		fmt.Printf("create chat model failed: %v\n", err)
		return
	}
	chatModel, err := cm.WithTools(toolDefs())
	if err != nil {
		fmt.Printf("bind tools failed: %v\n", err)
		return
	}

	system := fmt.Sprintf("You are a coding agent at %s. Use task tools to plan and track work.", workDir)
	history := []*schema.Message{schema.SystemMessage(system)}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\033[36ms07 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}
		history = append(history, schema.UserMessage(query))
		if err := agentLoop(ctx, chatModel, tm, &history); err != nil {
			fmt.Printf("agent loop error: %v\n\n", err)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Printf("read input failed: %v\n", err)
	}
}

func newClaudeModel(ctx context.Context) (*claude.ChatModel, error) {
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		return nil, fmt.Errorf("missing api key: set ANTHROPIC_API_KEY in .env")
	}
	modelID := strings.TrimSpace(os.Getenv("MODEL_ID"))
	if modelID == "" {
		return nil, fmt.Errorf("missing model id: set MODEL_ID in .env")
	}
	baseURL := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	var baseURLPtr *string
	if baseURL != "" {
		baseURLPtr = &baseURL
	}
	return claude.NewChatModel(ctx, &claude.Config{
		APIKey:    apiKey,
		BaseURL:   baseURLPtr,
		Model:     modelID,
		MaxTokens: 8000,
	})
}

func toolDefs() []*schema.ToolInfo {
	return []*schema.ToolInfo{
		{Name: "bash", Desc: "Run a shell command.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"command": {Type: schema.String, Required: true},
			})},
		{Name: "read_file", Desc: "Read file contents.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"path":  {Type: schema.String, Required: true},
				"limit": {Type: schema.Integer},
			})},
		{Name: "write_file", Desc: "Write content to file.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"path":    {Type: schema.String, Required: true},
				"content": {Type: schema.String, Required: true},
			})},
		{Name: "edit_file", Desc: "Replace exact text in file.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"path":     {Type: schema.String, Required: true},
				"old_text": {Type: schema.String, Required: true},
				"new_text": {Type: schema.String, Required: true},
			})},
		{Name: "task_create", Desc: "Create a new persistent task.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"subject":     {Type: schema.String, Required: true},
				"description": {Type: schema.String},
			})},
		{Name: "task_update", Desc: "Update a task status or dependencies.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"task_id":      {Type: schema.Integer, Required: true},
				"status":       {Type: schema.String, Desc: "pending | in_progress | completed"},
				"addBlockedBy": {Type: schema.Array, Desc: "task IDs that block this task"},
				"addBlocks":    {Type: schema.Array, Desc: "task IDs that this task blocks"},
			})},
		{Name: "task_list", Desc: "List all tasks with status summary.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{})},
		{Name: "task_get", Desc: "Get full details of a task by ID.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"task_id": {Type: schema.Integer, Required: true},
			})},
	}
}

func agentLoop(ctx context.Context, chatModel model.ToolCallingChatModel, tm *TaskManager, history *[]*schema.Message) error {
	dispatch := map[string]func(map[string]any) string{
		"bash":       func(kw map[string]any) string { return runBash(getString(kw, "command")) },
		"read_file":  func(kw map[string]any) string { return runRead(getString(kw, "path"), getInt(kw, "limit")) },
		"write_file": func(kw map[string]any) string { return runWrite(getString(kw, "path"), getString(kw, "content")) },
		"edit_file":  func(kw map[string]any) string { return runEdit(getString(kw, "path"), getString(kw, "old_text"), getString(kw, "new_text")) },
		"task_create": func(kw map[string]any) string {
			r, err := tm.Create(getString(kw, "subject"), getString(kw, "description"))
			if err != nil { return "Error: " + err.Error() }
			return r
		},
		"task_update": func(kw map[string]any) string {
			addBlockedBy := getIntSlice(kw, "addBlockedBy")
			addBlocks := getIntSlice(kw, "addBlocks")
			r, err := tm.Update(getInt(kw, "task_id"), getString(kw, "status"), addBlockedBy, addBlocks)
			if err != nil { return "Error: " + err.Error() }
			return r
		},
		"task_list": func(kw map[string]any) string { return tm.ListAll() },
		"task_get": func(kw map[string]any) string {
			r, err := tm.Get(getInt(kw, "task_id"))
			if err != nil { return "Error: " + err.Error() }
			return r
		},
	}
	for {
		resp, err := chatModel.Generate(ctx, *history)
		if err != nil { return err }
		*history = append(*history, resp)
		if len(resp.ToolCalls) == 0 {
			if strings.TrimSpace(resp.Content) != "" { fmt.Println(resp.Content) }
			fmt.Println()
			return nil
		}
		toolResults := make([]*schema.Message, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			args := map[string]any{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					output := "Error: Invalid tool arguments"
					fmt.Printf("> %s: %s\n", tc.Function.Name, output)
					toolResults = append(toolResults, schema.ToolMessage(output, tc.ID, schema.WithToolName(tc.Function.Name)))
					continue
				}
			}
			handler := dispatch[tc.Function.Name]
			output := "Unknown tool: " + tc.Function.Name
			if handler != nil { output = handler(args) }
			preview := output
			if len(preview) > 200 { preview = preview[:200] }
			fmt.Printf("> %s: %s\n", tc.Function.Name, preview)
			toolResults = append(toolResults, schema.ToolMessage(output, tc.ID, schema.WithToolName(tc.Function.Name)))
		}
		*history = append(*history, toolResults...)
	}
}
func safePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" { return "", fmt.Errorf("empty path") }
	candidate := filepath.Clean(filepath.Join(workDir, p))
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" { candidate = filepath.Clean(p) }
	base, err := canonicalPath(workDir)
	if err != nil { return "", err }
	target, err := canonicalPathForAccess(candidate)
	if err != nil { return "", err }
	if !isSubPath(base, target) { return "", fmt.Errorf("path escapes workspace: %s", p) }
	return target, nil
}
func canonicalPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil { return "", err }
	real, err := filepath.EvalSymlinks(abs)
	if err != nil { real = abs }
	return filepath.Clean(real), nil
}
func canonicalPathForAccess(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil { return "", err }
	if _, err := os.Stat(abs); err == nil {
		real, err := filepath.EvalSymlinks(abs)
		if err == nil { return filepath.Clean(real), nil }
		return filepath.Clean(abs), nil
	}
	dir := filepath.Dir(abs)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil { realDir = dir }
	return filepath.Clean(filepath.Join(realDir, filepath.Base(abs))), nil
}
func isSubPath(base, target string) bool {
	base = filepath.Clean(base); target = filepath.Clean(target)
	if runtime.GOOS == "windows" { base = strings.ToLower(base); target = strings.ToLower(target) }
	if base == target { return true }
	return strings.HasPrefix(target, base+string(filepath.Separator))
}
func runBash(command string) string {
	for _, d := range dangerousPatterns { if strings.Contains(command, d) { return "Error: Dangerous command blocked" } }
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" { cmd = exec.CommandContext(ctx, "powershell", "-Command", command)
	} else { cmd = exec.CommandContext(ctx, "bash", "-lc", command) }
	cmd.Dir = workDir
	out, _ := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded { return "Error: Timeout (120s)" }
	result := strings.TrimSpace(string(out))
	if result == "" { result = "(no output)" }
	if len(result) > maxToolOutput { result = result[:maxToolOutput] }
	return result
}
func runRead(path string, limit int) string {
	fp, err := safePath(path)
	if err != nil { return "Error: " + err.Error() }
	b, err := os.ReadFile(fp)
	if err != nil { return "Error: " + err.Error() }
	lines := strings.Split(string(b), "\n")
	if limit > 0 && limit < len(lines) {
		left := len(lines) - limit
		lines = append(lines[:limit], fmt.Sprintf("... (%d more lines)", left))
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxToolOutput { out = out[:maxToolOutput] }
	if out == "" { return "(no output)" }
	return out
}
func runWrite(path, content string) string {
	fp, err := safePath(path)
	if err != nil { return "Error: " + err.Error() }
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil { return "Error: " + err.Error() }
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil { return "Error: " + err.Error() }
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}
func runEdit(path, oldText, newText string) string {
	fp, err := safePath(path)
	if err != nil { return "Error: " + err.Error() }
	b, err := os.ReadFile(fp)
	if err != nil { return "Error: " + err.Error() }
	content := string(b)
	idx := strings.Index(content, oldText)
	if idx < 0 { return fmt.Sprintf("Error: Text not found in %s", path) }
	edited := content[:idx] + newText + content[idx+len(oldText):]
	if err := os.WriteFile(fp, []byte(edited), 0o644); err != nil { return "Error: " + err.Error() }
	return "Edited " + path
}
func getString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil { return "" }
	if s, ok := v.(string); ok { return s }
	return fmt.Sprint(v)
}
func getInt(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok || v == nil { return 0 }
	switch t := v.(type) {
	case float64: return int(t)
	case int: return t
	case int64: return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	default: return 0
	}
}
func getIntSlice(m map[string]any, key string) []int {
	v, ok := m[key]
	if !ok || v == nil { return nil }
	raw, ok := v.([]interface{})
	if !ok { return nil }
	out := make([]int, 0, len(raw))
	for _, elem := range raw {
		switch t := elem.(type) {
		case float64: out = append(out, int(t))
		case int: out = append(out, t)
		case int64: out = append(out, int(t))
		}
	}
	return out
}
