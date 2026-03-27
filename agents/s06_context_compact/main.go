package main

// Harness: compression -- clean memory for infinite sessions.
// s06_context_compact - Compact
//
// Three-layer compression pipeline so the agent can work forever:
//
//	Every turn:
//	+------------------+
//	| Tool call result |
//	+------------------+
//	        |
//	        v
//	[Layer 1: microCompact]        (silent, every turn)
//	  Replace tool_result content older than last 3
//	  with "[Previous: used {tool_name}]"
//	        |
//	        v
//	[Check: tokens > 50000?]
//	   |               |
//	   no              yes
//	   |               |
//	   v               v
//	continue    [Layer 2: autoCompact]
//	              Save full transcript to .transcripts/
//	              Ask LLM to summarize conversation.
//	              Replace all messages with [summary].
//	                    |
//	                    v
//	            [Layer 3: compact tool]
//	              Model calls compact -> immediate summarization.
//	              Same as auto, triggered manually.
//
// Key insight: "The agent can forget strategically and keep working forever."

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/joho/godotenv"
)

const (
	maxToolOutput  = 50000
	toolTimeout    = 120 * time.Second
	tokenThreshold = 50000
	keepRecent     = 3
	transcriptDir  = ".transcripts"
)

var (
	dangerousPatterns = []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	workDir           string
)

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

	system := fmt.Sprintf("You are a coding agent at %s. Use tools to solve tasks. Call compact when you want to compress the conversation.", workDir)
	history := []*schema.Message{schema.SystemMessage(system)}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\033[36ms06 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}
		history = append(history, schema.UserMessage(query))
		if err := agentLoop(ctx, cm, chatModel, &history); err != nil {
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
		{Name: "compact", Desc: "Trigger manual conversation compression to free up context.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"focus": {Type: schema.String, Desc: "What to preserve in the summary"},
			})},
	}
}

// estimateTokens: rough token count (~4 chars per token)
func estimateTokens(history []*schema.Message) int {
	total := 0
	for _, m := range history {
		total += len(m.Content)
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments)
		}
	}
	return total / 4
}

// Layer 1: microCompact - replace old tool results with short placeholders
func microCompact(history []*schema.Message) {
	type entry struct {
		idx     int
		msg     *schema.Message
		oldBody string
		name    string
	}
	// build tool_name map from ToolCalls
	toolNameMap := map[string]string{}
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			toolNameMap[tc.ID] = tc.Function.Name
		}
	}
	// collect tool messages
	var toolMsgs []entry
	for i, m := range history {
		if m.Role == schema.Tool && len(m.Content) > 100 {
			toolMsgs = append(toolMsgs, entry{
				idx:     i,
				msg:     m,
				oldBody: m.Content,
				name:    toolNameMap[m.ToolCallID],
			})
		}
	}
	if len(toolMsgs) <= keepRecent {
		return
	}
	// clear all but the last keepRecent
	for _, e := range toolMsgs[:len(toolMsgs)-keepRecent] {
		name := e.name
		if name == "" {
			name = "unknown"
		}
		e.msg.Content = fmt.Sprintf("[Previous: used %s]", name)
		fmt.Printf("  [micro_compact] cleared: %s\n", name)
	}
}

// Layer 2 & 3: autoCompact - save transcript, summarize, replace history
func autoCompact(ctx context.Context, cm *claude.ChatModel, history []*schema.Message) ([]*schema.Message, error) {
	// Save transcript
	if err := os.MkdirAll(filepath.Join(workDir, transcriptDir), 0o755); err != nil {
		return history, err
	}
	transcriptPath := filepath.Join(workDir, transcriptDir, fmt.Sprintf("transcript_%d.jsonl", time.Now().Unix()))
	f, err := os.Create(transcriptPath)
	if err != nil {
		return history, err
	}
	enc := json.NewEncoder(f)
	for _, m := range history {
		_ = enc.Encode(m)
	}
	f.Close()
	fmt.Printf("  [transcript saved: %s]\n", transcriptPath)

	// Build conversation text for summarization (cap at 80000 chars)
	var sb strings.Builder
	for _, m := range history {
		sb.WriteString(string(m.Role))
		sb.WriteString(": ")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
		if sb.Len() > 80000 {
			break
		}
	}

	// Ask LLM to summarize using a plain model (no tools)
	plainModel, err := cm.WithTools(nil)
	if err != nil {
		plainModel = cm
	}
	sumPrompt := "Summarize this conversation for continuity. Include: " +
		"1) What was accomplished, 2) Current state, 3) Key decisions made. " +
		"Be concise but preserve critical details.\n\n" + sb.String()
	sumResp, err := plainModel.Generate(ctx, []*schema.Message{
		schema.UserMessage(sumPrompt),
	})
	if err != nil {
		return history, fmt.Errorf("summarization failed: %w", err)
	}
	summary := strings.TrimSpace(sumResp.Content)

	// Replace all messages with compressed summary
	compressed := []*schema.Message{
		schema.UserMessage(fmt.Sprintf("[Conversation compressed. Transcript: %s]\n\n%s", transcriptPath, summary)),
		schema.AssistantMessage("Understood. I have the context from the summary. Continuing.", nil),
	}
	fmt.Println("  [context compacted]")
	return compressed, nil
}

func agentLoop(ctx context.Context, cm *claude.ChatModel, chatModel model.ToolCallingChatModel, history *[]*schema.Message) error {
	dispatch := map[string]func(map[string]any) string{
		"bash":       func(kw map[string]any) string { return runBash(getString(kw, "command")) },
		"read_file":  func(kw map[string]any) string { return runRead(getString(kw, "path"), getInt(kw, "limit")) },
		"write_file": func(kw map[string]any) string { return runWrite(getString(kw, "path"), getString(kw, "content")) },
		"edit_file":  func(kw map[string]any) string { return runEdit(getString(kw, "path"), getString(kw, "old_text"), getString(kw, "new_text")) },
		"compact":    func(kw map[string]any) string { return "Compressing..." },
	}
	for {
		// Layer 1: micro_compact before each LLM call
		microCompact(*history)
		// Layer 2: auto_compact if token estimate exceeds threshold
		if estimateTokens(*history) > tokenThreshold {
			fmt.Println("[auto_compact triggered]")
			compacted, err := autoCompact(ctx, cm, *history)
			if err != nil {
				fmt.Printf("  [auto_compact error: %v]\n", err)
			} else {
				*history = compacted
			}
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
		manualCompact := false
		for _, tc := range resp.ToolCalls {
			args := map[string]any{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					output := "Error: Invalid tool arguments"
					toolResults = append(toolResults, schema.ToolMessage(output, tc.ID, schema.WithToolName(tc.Function.Name)))
					continue
				}
			}
			if tc.Function.Name == "compact" { manualCompact = true }
			handler := dispatch[tc.Function.Name]
			output := "Unknown tool: " + tc.Function.Name
			if handler != nil { output = handler(args) }
			preview := output
			if len(preview) > 200 { preview = preview[:200] }
			fmt.Printf("> %s: %s\n", tc.Function.Name, preview)
			toolResults = append(toolResults, schema.ToolMessage(output, tc.ID, schema.WithToolName(tc.Function.Name)))
		}
		*history = append(*history, toolResults...)
		// Layer 3: manual compact triggered by compact tool
		if manualCompact {
			fmt.Println("[manual compact]")
			compacted, err := autoCompact(ctx, cm, *history)
			if err != nil {
				fmt.Printf("  [compact error: %v]\n", err)
			} else {
				*history = compacted
			}
		}
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
