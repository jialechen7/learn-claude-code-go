package main

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
	"sync"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/joho/godotenv"
)

const (
	maxToolOutput = 50000
	toolTimeout   = 120 * time.Second
	teammateLimit = 50
)

var (
	dangerousPatterns = []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	workDir           string
	validMsgTypes     = map[string]bool{
		"message": true, "broadcast": true,
		"shutdown_request": true, "shutdown_response": true,
		"plan_approval_response": true,
	}
)

type Message struct {
	Type      string  `json:"type"`
	From      string  `json:"from"`
	Content   string  `json:"content"`
	Timestamp float64 `json:"timestamp"`
}

type MessageBus struct {
	dir string
	mu  sync.Mutex
}

func NewMessageBus(dir string) (*MessageBus, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &MessageBus{dir: dir}, nil
}

func (mb *MessageBus) Send(sender, to, content, msgType string) string {
	if !validMsgTypes[msgType] {
		return fmt.Sprintf("Error: Invalid type '%s'", msgType)
	}
	msg := Message{Type: msgType, From: sender, Content: content,
		Timestamp: float64(time.Now().UnixNano()) / 1e9}
	b, _ := json.Marshal(msg)
	mb.mu.Lock()
	defer mb.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(mb.dir, to+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "Error: " + err.Error()
	}
	defer f.Close()
	f.Write(b)
	f.WriteString("\n")
	return fmt.Sprintf("Sent %s to %s", msgType, to)
}

func (mb *MessageBus) ReadInbox(name string) []Message {
	path := filepath.Join(mb.dir, name+".jsonl")
	mb.mu.Lock()
	defer mb.mu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var msgs []Message
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var m Message
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			msgs = append(msgs, m)
		}
	}
	_ = os.WriteFile(path, []byte{}, 0o644)
	return msgs
}

func (mb *MessageBus) Broadcast(sender, content string, names []string) string {
	count := 0
	for _, name := range names {
		if name != sender {
			mb.Send(sender, name, content, "broadcast")
			count++
		}
	}
	return fmt.Sprintf("Broadcast to %d teammates", count)
}

type Member struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type TeamConfig struct {
	TeamName string   `json:"team_name"`
	Members  []Member `json:"members"`
}

type TeammateManager struct {
	dir        string
	configPath string
	mu         sync.Mutex
	config     TeamConfig
	bus        *MessageBus
	cm         *claude.ChatModel
}

func NewTeammateManager(dir string, bus *MessageBus, cm *claude.ChatModel) (*TeammateManager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tm := &TeammateManager{dir: dir, configPath: filepath.Join(dir, "config.json"), bus: bus, cm: cm}
	tm.loadConfig()
	return tm, nil
}

func (tm *TeammateManager) loadConfig() {
	b, err := os.ReadFile(tm.configPath)
	if err != nil {
		tm.config = TeamConfig{TeamName: "default", Members: []Member{}}
		return
	}
	_ = json.Unmarshal(b, &tm.config)
}

func (tm *TeammateManager) saveConfig() {
	b, _ := json.MarshalIndent(tm.config, "", "  ")
	_ = os.WriteFile(tm.configPath, b, 0o644)
}

func (tm *TeammateManager) findMember(name string) *Member {
	for i := range tm.config.Members {
		if tm.config.Members[i].Name == name {
			return &tm.config.Members[i]
		}
	}
	return nil
}

func (tm *TeammateManager) Spawn(name, role, prompt string) string {
	tm.mu.Lock()
	member := tm.findMember(name)
	if member != nil {
		if member.Status != "idle" && member.Status != "shutdown" {
			tm.mu.Unlock()
			return fmt.Sprintf("Error: '%s' is currently %s", name, member.Status)
		}
		member.Status = "working"
		member.Role = role
	} else {
		tm.config.Members = append(tm.config.Members, Member{Name: name, Role: role, Status: "working"})
	}
	tm.saveConfig()
	tm.mu.Unlock()
	go tm.teammateLoop(name, role, prompt)
	return fmt.Sprintf("Spawned '%s' (role: %s)", name, role)
}

func (tm *TeammateManager) teammateLoop(name, role, prompt string) {
	sysPrompt := fmt.Sprintf("You are '%s', role: %s, at %s. Use send_message to communicate. Complete your task.", name, role, workDir)
	childModel, err := tm.cm.WithTools(teammateToolDefs())
	if err != nil {
		fmt.Printf("  [%s] tool bind failed: %v\n", name, err)
		return
	}
	history := []*schema.Message{schema.SystemMessage(sysPrompt), schema.UserMessage(prompt)}
	dispatch := tm.teammateDispatch(name)
	ctx := context.Background()
	for i := 0; i < teammateLimit; i++ {
		if msgs := tm.bus.ReadInbox(name); len(msgs) > 0 {
			b, _ := json.MarshalIndent(msgs, "", "  ")
			history = append(history, schema.UserMessage(string(b)))
		}
		resp, err := childModel.Generate(ctx, history)
		if err != nil {
			fmt.Printf("  [%s] error: %v\n", name, err)
			break
		}
		history = append(history, resp)
		if len(resp.ToolCalls) == 0 {
			break
		}
		toolResults := make([]*schema.Message, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			args := map[string]any{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}
			handler := dispatch[tc.Function.Name]
			output := "Unknown tool: " + tc.Function.Name
			if handler != nil {
				output = handler(args)
			}
			fmt.Printf("  [%s] > %s: %s\n", name, tc.Function.Name, truncate(output, 120))
			toolResults = append(toolResults, schema.ToolMessage(output, tc.ID, schema.WithToolName(tc.Function.Name)))
		}
		history = append(history, toolResults...)
	}
	tm.mu.Lock()
	if m := tm.findMember(name); m != nil && m.Status != "shutdown" {
		m.Status = "idle"
		tm.saveConfig()
	}
	tm.mu.Unlock()
}

func (tm *TeammateManager) teammateDispatch(name string) map[string]func(map[string]any) string {
	return map[string]func(map[string]any) string{
		"bash":       func(kw map[string]any) string { return runBash(getString(kw, "command")) },
		"read_file":  func(kw map[string]any) string { return runRead(getString(kw, "path"), getInt(kw, "limit")) },
		"write_file": func(kw map[string]any) string { return runWrite(getString(kw, "path"), getString(kw, "content")) },
		"edit_file":  func(kw map[string]any) string { return runEdit(getString(kw, "path"), getString(kw, "old_text"), getString(kw, "new_text")) },
		"send_message": func(kw map[string]any) string {
			mt := getString(kw, "msg_type")
			if mt == "" {
				mt = "message"
			}
			return tm.bus.Send(name, getString(kw, "to"), getString(kw, "content"), mt)
		},
		"read_inbox": func(kw map[string]any) string {
			msgs := tm.bus.ReadInbox(name)
			b, _ := json.MarshalIndent(msgs, "", "  ")
			return string(b)
		},
	}
}

func (tm *TeammateManager) ListAll() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if len(tm.config.Members) == 0 {
		return "No teammates."
	}
	lines := []string{fmt.Sprintf("Team: %s", tm.config.TeamName)}
	for _, m := range tm.config.Members {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", m.Name, m.Role, m.Status))
	}
	return strings.Join(lines, "\n")
}

func (tm *TeammateManager) MemberNames() []string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	names := make([]string, 0, len(tm.config.Members))
	for _, m := range tm.config.Members {
		names = append(names, m.Name)
	}
	return names
}

func teammateToolDefs() []*schema.ToolInfo {
	return []*schema.ToolInfo{
		{Name: "bash", Desc: "Run a shell command.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"command": {Type: schema.String, Required: true}})},
		{Name: "read_file", Desc: "Read file contents.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"path": {Type: schema.String, Required: true}, "limit": {Type: schema.Integer}})},
		{Name: "write_file", Desc: "Write content to file.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"path": {Type: schema.String, Required: true}, "content": {Type: schema.String, Required: true}})},
		{Name: "edit_file", Desc: "Replace exact text in file.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"path": {Type: schema.String, Required: true}, "old_text": {Type: schema.String, Required: true}, "new_text": {Type: schema.String, Required: true}})},
		{Name: "send_message", Desc: "Send message to a teammate.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"to": {Type: schema.String, Required: true}, "content": {Type: schema.String, Required: true},
				"msg_type": {Type: schema.String, Desc: "message|broadcast|shutdown_request|shutdown_response|plan_approval_response"}})},
		{Name: "read_inbox", Desc: "Read and drain your inbox.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{})},
	}
}


func main() {
	if err := godotenv.Overload(); err != nil && !os.IsNotExist(err) { fmt.Printf("load .env failed: %v\n", err); return }
	ctx := context.Background()
	wd, err := os.Getwd()
	if err != nil { fmt.Printf("get cwd failed: %v\n", err); return }
	workDir = wd
	cm, err := newClaudeModel(ctx)
	if err != nil { fmt.Printf("create chat model failed: %v\n", err); return }
	bus, err := NewMessageBus(filepath.Join(workDir, ".team", "inbox"))
	if err != nil { fmt.Printf("init message bus failed: %v\n", err); return }
	tm, err := NewTeammateManager(filepath.Join(workDir, ".team"), bus, cm)
	if err != nil { fmt.Printf("init team manager failed: %v\n", err); return }
	chatModel, err := cm.WithTools(leadToolDefs())
	if err != nil { fmt.Printf("bind tools failed: %v\n", err); return }
	system := fmt.Sprintf("You are a team lead at %s. Spawn teammates and communicate via inboxes.", workDir)
	history := []*schema.Message{schema.SystemMessage(system)}
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\033[36ms09 >> \033[0m")
		if !scanner.Scan() { break }
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") { break }
		if query == "/team" { fmt.Println(tm.ListAll()); continue }
		if query == "/inbox" {
			msgs := bus.ReadInbox("lead")
			b, _ := json.MarshalIndent(msgs, "", "  ")
			fmt.Println(string(b)); continue
		}
		history = append(history, schema.UserMessage(query))
		if err := agentLoop(ctx, chatModel, bus, tm, &history); err != nil {
			fmt.Printf("agent loop error: %v\n\n", err)
		}
	}
	if err := scanner.Err(); err != nil { fmt.Printf("read input failed: %v\n", err) }
}
func newClaudeModel(ctx context.Context) (*claude.ChatModel, error) {
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" { return nil, fmt.Errorf("missing api key") }
	modelID := strings.TrimSpace(os.Getenv("MODEL_ID"))
	if modelID == "" { return nil, fmt.Errorf("missing model id") }
	baseURL := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	var baseURLPtr *string
	if baseURL != "" { baseURLPtr = &baseURL }
	return claude.NewChatModel(ctx, &claude.Config{APIKey: apiKey, BaseURL: baseURLPtr, Model: modelID, MaxTokens: 8000})
}
func leadToolDefs() []*schema.ToolInfo {
	return []*schema.ToolInfo{
		{Name: "bash", Desc: "Run a shell command.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"command": {Type: schema.String, Required: true}})},
		{Name: "read_file", Desc: "Read file contents.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"path": {Type: schema.String, Required: true}, "limit": {Type: schema.Integer}})},
		{Name: "write_file", Desc: "Write content to file.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"path": {Type: schema.String, Required: true}, "content": {Type: schema.String, Required: true}})},
		{Name: "edit_file", Desc: "Replace exact text in file.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"path": {Type: schema.String, Required: true}, "old_text": {Type: schema.String, Required: true}, "new_text": {Type: schema.String, Required: true}})},
		{Name: "spawn_teammate", Desc: "Spawn a persistent teammate goroutine.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"name": {Type: schema.String, Required: true}, "role": {Type: schema.String, Required: true}, "prompt": {Type: schema.String, Required: true}})},
		{Name: "list_teammates", Desc: "List all teammates.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{})},
		{Name: "send_message", Desc: "Send a message to a teammate.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"to": {Type: schema.String, Required: true}, "content": {Type: schema.String, Required: true}, "msg_type": {Type: schema.String}})},
		{Name: "read_inbox", Desc: "Read and drain lead inbox.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{})},
		{Name: "broadcast", Desc: "Send to all teammates.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"content": {Type: schema.String, Required: true}})},
	}
}
func agentLoop(ctx context.Context, chatModel model.ToolCallingChatModel, bus *MessageBus, tm *TeammateManager, history *[]*schema.Message) error {
	dispatch := map[string]func(map[string]any) string{
		"bash":           func(kw map[string]any) string { return runBash(getString(kw, "command")) },
		"read_file":      func(kw map[string]any) string { return runRead(getString(kw, "path"), getInt(kw, "limit")) },
		"write_file":     func(kw map[string]any) string { return runWrite(getString(kw, "path"), getString(kw, "content")) },
		"edit_file":      func(kw map[string]any) string { return runEdit(getString(kw, "path"), getString(kw, "old_text"), getString(kw, "new_text")) },
		"spawn_teammate": func(kw map[string]any) string { return tm.Spawn(getString(kw, "name"), getString(kw, "role"), getString(kw, "prompt")) },
		"list_teammates": func(kw map[string]any) string { return tm.ListAll() },
		"send_message": func(kw map[string]any) string {
			mt := getString(kw, "msg_type"); if mt == "" { mt = "message" }
			return bus.Send("lead", getString(kw, "to"), getString(kw, "content"), mt)
		},
		"read_inbox": func(kw map[string]any) string {
			msgs := bus.ReadInbox("lead"); b, _ := json.MarshalIndent(msgs, "", "  "); return string(b)
		},
		"broadcast": func(kw map[string]any) string { return bus.Broadcast("lead", getString(kw, "content"), tm.MemberNames()) },
	}
	for {
		if msgs := bus.ReadInbox("lead"); len(msgs) > 0 {
			b, _ := json.MarshalIndent(msgs, "", "  ")
			*history = append(*history, schema.UserMessage("<inbox>\n"+string(b)+"\n</inbox>"), schema.AssistantMessage("Noted inbox messages.", nil))
		}
		resp, err := chatModel.Generate(ctx, *history)
		if err != nil { return err }
		*history = append(*history, resp)
		if len(resp.ToolCalls) == 0 {
			if strings.TrimSpace(resp.Content) != "" { fmt.Println(resp.Content) }
			fmt.Println(); return nil
		}
		toolResults := make([]*schema.Message, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			args := map[string]any{}
			if strings.TrimSpace(tc.Function.Arguments) != "" { _ = json.Unmarshal([]byte(tc.Function.Arguments), &args) }
			handler := dispatch[tc.Function.Name]
			output := "Unknown tool: " + tc.Function.Name
			if handler != nil { output = handler(args) }
			fmt.Printf("> %s: %s\n", tc.Function.Name, truncate(output, 200))
			toolResults = append(toolResults, schema.ToolMessage(output, tc.ID, schema.WithToolName(tc.Function.Name)))
		}
		*history = append(*history, toolResults...)
	}
}
func truncate(s string, n int) string { if len(s) <= n { return s }; return s[:n] + "..." }
func safePath(p string) (string, error) {
	p = strings.TrimSpace(p); if p == "" { return "", fmt.Errorf("empty path") }
	candidate := filepath.Clean(filepath.Join(workDir, p))
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" { candidate = filepath.Clean(p) }
	base, _ := filepath.Abs(workDir); target, _ := filepath.Abs(candidate)
	if runtime.GOOS == "windows" { base = strings.ToLower(base); target = strings.ToLower(target) }
	if !strings.HasPrefix(target, base+string(filepath.Separator)) && base != target { return "", fmt.Errorf("path escapes workspace: %s", p) }
	return target, nil
}
func runBash(command string) string {
	for _, d := range dangerousPatterns { if strings.Contains(command, d) { return "Error: Dangerous command blocked" } }
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout); defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" { cmd = exec.CommandContext(ctx, "powershell", "-Command", command)
	} else { cmd = exec.CommandContext(ctx, "bash", "-lc", command) }
	cmd.Dir = workDir; out, _ := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded { return "Error: Timeout (120s)" }
	result := strings.TrimSpace(string(out))
	if result == "" { result = "(no output)" }
	if len(result) > maxToolOutput { result = result[:maxToolOutput] }
	return result
}
func runRead(path string, limit int) string {
	fp, err := safePath(path); if err != nil { return "Error: " + err.Error() }
	b, err := os.ReadFile(fp); if err != nil { return "Error: " + err.Error() }
	lines := strings.Split(string(b), "\n")
	if limit > 0 && limit < len(lines) { left := len(lines) - limit; lines = append(lines[:limit], fmt.Sprintf("... (%d more lines)", left)) }
	out := strings.Join(lines, "\n")
	if len(out) > maxToolOutput { out = out[:maxToolOutput] }
	if out == "" { return "(no output)" }; return out
}
func runWrite(path, content string) string {
	fp, err := safePath(path); if err != nil { return "Error: " + err.Error() }
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil { return "Error: " + err.Error() }
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil { return "Error: " + err.Error() }
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}
func runEdit(path, oldText, newText string) string {
	fp, err := safePath(path); if err != nil { return "Error: " + err.Error() }
	b, err := os.ReadFile(fp); if err != nil { return "Error: " + err.Error() }
	content := string(b)
	idx := strings.Index(content, oldText)
	if idx < 0 { return fmt.Sprintf("Error: Text not found in %s", path) }
	edited := content[:idx] + newText + content[idx+len(oldText):]
	if err := os.WriteFile(fp, []byte(edited), 0o644); err != nil { return "Error: " + err.Error() }
	return "Edited " + path
}
func getString(m map[string]any, key string) string {
	v, ok := m[key]; if !ok || v == nil { return "" }
	if s, ok := v.(string); ok { return s }; return fmt.Sprint(v)
}
func getInt(m map[string]any, key string) int {
	v, ok := m[key]; if !ok || v == nil { return 0 }
	switch t := v.(type) {
	case float64: return int(t)
	case int: return t
	case int64: return int(t)
	case string: n, _ := strconv.Atoi(strings.TrimSpace(t)); return n
	default: return 0
	}
}
