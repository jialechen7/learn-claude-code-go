package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	maxToolOutput = 50000
	toolTimeout   = 120 * time.Second
)

var (
	dangerousPatterns = []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	workDir           string
)

// -- SkillLoader: scan skills/<name>/SKILL.md with YAML frontmatter --

type Skill struct {
	Meta map[string]string
	Body string
	Path string
}

type SkillLoader struct {
	skillsDir string
	skills    map[string]*Skill
}

func NewSkillLoader(dir string) *SkillLoader {
	sl := &SkillLoader{skillsDir: dir, skills: map[string]*Skill{}}
	sl.loadAll()
	return sl
}

func (sl *SkillLoader) loadAll() {
	if _, err := os.Stat(sl.skillsDir); os.IsNotExist(err) {
		return
	}
	_ = filepath.WalkDir(sl.skillsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		meta, body := parseFrontmatter(string(b))
		name := meta["name"]
		if name == "" {
			name = filepath.Base(filepath.Dir(path))
		}
		sl.skills[name] = &Skill{Meta: meta, Body: body, Path: path}
		return nil
	})
}

func parseFrontmatter(text string) (map[string]string, string) {
	re := regexp.MustCompile(`(?s)^---\n(.*?)\n---\n(.*)`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		return map[string]string{}, text
	}
	meta := map[string]string{}
	for _, line := range strings.Split(m[1], "\n") {
		if idx := strings.Index(line, ":"); idx >= 0 {
			meta[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
		}
	}
	return meta, strings.TrimSpace(m[2])
}

// Layer 1: short descriptions for system prompt (~100 tokens/skill)
func (sl *SkillLoader) GetDescriptions() string {
	if len(sl.skills) == 0 {
		return "(no skills available)"
	}
	var lines []string
	for name, skill := range sl.skills {
		desc := skill.Meta["description"]
		if desc == "" {
			desc = "No description"
		}
		line := fmt.Sprintf("  - %s: %s", name, desc)
		if tags := skill.Meta["tags"]; tags != "" {
			line += fmt.Sprintf(" [%s]", tags)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// Layer 2: full skill body returned in tool_result
func (sl *SkillLoader) GetContent(name string) string {
	skill, ok := sl.skills[name]
	if !ok {
		avail := make([]string, 0, len(sl.skills))
		for k := range sl.skills {
			avail = append(avail, k)
		}
		return fmt.Sprintf("Error: Unknown skill '%s'. Available: %s", name, strings.Join(avail, ", "))
	}
	return fmt.Sprintf("<skill name=\"%s\">\n%s\n</skill>", name, skill.Body)
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

	skillsDir := filepath.Join(workDir, "skills")
	loader := NewSkillLoader(skillsDir)

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

	// Layer 1: skill metadata injected into system prompt
	system := fmt.Sprintf(
		"You are a coding agent at %s.\n"+
			"Use load_skill to access specialized knowledge before tackling unfamiliar topics.\n\n"+
			"Skills available:\n%s",
		workDir, loader.GetDescriptions(),
	)
	history := []*schema.Message{schema.SystemMessage(system)}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\033[36ms05 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}
		history = append(history, schema.UserMessage(query))
		if err := agentLoop(ctx, chatModel, loader, &history); err != nil {
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
		{Name: "load_skill", Desc: "Load specialized knowledge by name. Call this before working on unfamiliar topics.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"name": {Type: schema.String, Required: true, Desc: "Skill name to load"},
			})},
	}
}

func agentLoop(ctx context.Context, chatModel model.ToolCallingChatModel, loader *SkillLoader, history *[]*schema.Message) error {
	dispatch := map[string]func(map[string]any) string{
		"bash":       func(kw map[string]any) string { return runBash(getString(kw, "command")) },
		"read_file":  func(kw map[string]any) string { return runRead(getString(kw, "path"), getInt(kw, "limit")) },
		"write_file": func(kw map[string]any) string { return runWrite(getString(kw, "path"), getString(kw, "content")) },
		"edit_file":  func(kw map[string]any) string { return runEdit(getString(kw, "path"), getString(kw, "old_text"), getString(kw, "new_text")) },
		"load_skill": func(kw map[string]any) string { return loader.GetContent(getString(kw, "name")) },
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
