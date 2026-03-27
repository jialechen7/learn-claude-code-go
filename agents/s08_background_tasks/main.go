package main

// Harness: background execution -- the model thinks while the harness waits.
// s08_background_tasks - Background Tasks
//
// Run commands in background goroutines. A notification channel is drained
// before each LLM call to deliver results.
//
//	Main goroutine              Background goroutine
//	+-----------------+        +-----------------+
//	| agent loop      |        | task executes   |
//	| ...             |        | ...             |
//	| [LLM call] <---+------- | enqueue(result) |
//	|  ^drain queue   |        +-----------------+
//	+-----------------+
//
// Key insight: "Fire and forget -- the agent doesn't block while the command runs."

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/joho/godotenv"
)

const (
	maxToolOutput  = 50000
	toolTimeout    = 120 * time.Second
	bgTaskTimeout  = 300 * time.Second
)

var (
	dangerousPatterns = []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	workDir           string
)

// -- BackgroundManager: goroutine-based execution + notification channel --

type BgTask struct {
	ID      string
	Command string
	Status  string // running | completed | timeout | error
	Result  string
}

type BgNotification struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Command string `json:"command"`
	Result  string `json:"result"`
}

type BackgroundManager struct {
	mu    sync.Mutex
	tasks map[string]*BgTask
	queue []BgNotification
}

func NewBackgroundManager() *BackgroundManager {
	return &BackgroundManager{
		tasks: map[string]*BgTask{},
	}
}

func (bm *BackgroundManager) Run(command string) string {
	taskID := randomID()
	task := &BgTask{ID: taskID, Command: command, Status: "running"}
	bm.mu.Lock()
	bm.tasks[taskID] = task
	bm.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), bgTaskTimeout)
		defer cancel()
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "powershell", "-Command", command)
		} else {
			cmd = exec.CommandContext(ctx, "bash", "-lc", command)
		}
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		output := strings.TrimSpace(string(out))
		if len(output) > maxToolOutput {
			output = output[:maxToolOutput]
		}
		if output == "" {
			output = "(no output)"
		}
		status := "completed"
		if ctx.Err() == context.DeadlineExceeded {
			status = "timeout"
			output = "Error: Timeout (300s)"
		} else if err != nil && len(out) == 0 {
			status = "error"
			output = "Error: " + err.Error()
		}
		bm.mu.Lock()
		task.Status = status
		task.Result = output
		preview := output
		if len(preview) > 500 {
			preview = preview[:500]
		}
		bm.queue = append(bm.queue, BgNotification{
			TaskID:  taskID,
			Status:  status,
			Command: truncate(command, 80),
			Result:  preview,
		})
		bm.mu.Unlock()
	}()

	return fmt.Sprintf("Background task %s started: %s", taskID, truncate(command, 80))
}

func (bm *BackgroundManager) Check(taskID string) string {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if taskID != "" {
		t, ok := bm.tasks[taskID]
		if !ok {
			return fmt.Sprintf("Error: Unknown task %s", taskID)
		}
		result := t.Result
		if result == "" {
			result = "(running)"
		}
		return fmt.Sprintf("[%s] %s\n%s", t.Status, truncate(t.Command, 60), result)
	}
	if len(bm.tasks) == 0 {
		return "No background tasks."
	}
	var lines []string
	for tid, t := range bm.tasks {
		lines = append(lines, fmt.Sprintf("%s: [%s] %s", tid, t.Status, truncate(t.Command, 60)))
	}
	return strings.Join(lines, "\n")
}

func (bm *BackgroundManager) DrainNotifications() []BgNotification {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	notifs := make([]BgNotification, len(bm.queue))
	copy(notifs, bm.queue)
	bm.queue = bm.queue[:0]
	return notifs
}

func randomID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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

	bg := NewBackgroundManager()

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

	system := fmt.Sprintf("You are a coding agent at %s. Use background_run for long-running commands.", workDir)
	history := []*schema.Message{schema.SystemMessage(system)}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\033[36ms08 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}
		history = append(history, schema.UserMessage(query))
		if err := agentLoop(ctx, chatModel, bg, &history); err != nil {
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
		{Name: "bash", Desc: "Run a shell command (blocking).",
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
		{Name: "background_run", Desc: "Run command in background goroutine. Returns task_id immediately.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"command": {Type: schema.String, Required: true},
			})},
		{Name: "check_background", Desc: "Check background task status. Omit task_id to list all.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"task_id": {Type: schema.String},
			})},
	}
}

func agentLoop(ctx context.Context, chatModel model.ToolCallingChatModel, bg *BackgroundManager, history *[]*schema.Message) error {
	dispatch := map[string]func(map[string]any) string{
		"bash":             func(kw map[string]any) string { return runBash(getString(kw, "command")) },
		"read_file":        func(kw map[string]any) string { return runRead(getString(kw, "path"), getInt(kw, "limit")) },
		"write_file":       func(kw map[string]any) string { return runWrite(getString(kw, "path"), getString(kw, "content")) },
		"edit_file":        func(kw map[string]any) string { return runEdit(getString(kw, "path"), getString(kw, "old_text"), getString(kw, "new_text")) },
		"background_run":   func(kw map[string]any) string { return bg.Run(getString(kw, "command")) },
		"check_background": func(kw map[string]any) string { return bg.Check(getString(kw, "task_id")) },
	}
	for {
		// Drain background notifications and inject before LLM call
		if notifs := bg.DrainNotifications(); len(notifs) > 0 {
			var sb strings.Builder
			for _, n := range notifs {
				fmt.Fprintf(&sb, "[bg:%s] %s: %s\n", n.TaskID, n.Status, n.Result)
			}
			*history = append(*history,
				schema.UserMessage("<background-results>\n"+sb.String()+"</background-results>"),
				schema.AssistantMessage("Noted background results.", nil),
			)
		}
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
