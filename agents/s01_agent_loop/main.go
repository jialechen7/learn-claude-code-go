package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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

var dangerousPatterns = []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}

type bashInput struct {
	Command string `json:"command"`
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

	cm, err := newClaudeModel(ctx)
	if err != nil {
		fmt.Printf("create chat model failed: %v\n", err)
		return
	}

	chatModel, err := cm.WithTools([]*schema.ToolInfo{
		{
			Name: "bash",
			Desc: "Run a shell command.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"command": {
					Type:     schema.String,
					Desc:     "The shell command to run.",
					Required: true,
				},
			}),
		},
	})
	if err != nil {
		fmt.Printf("bind tools failed: %v\n", err)
		return
	}

	system := fmt.Sprintf("You are a coding agent at %s. Use bash to solve tasks. Act, don't explain.", wd)
	history := []*schema.Message{schema.SystemMessage(system)}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\033[36ms01 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}

		history = append(history, schema.UserMessage(query))
		if err := agentLoop(ctx, chatModel, &history); err != nil {
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

func agentLoop(ctx context.Context, chatModel model.ToolCallingChatModel, history *[]*schema.Message) error {
	for {
		resp, err := chatModel.Generate(ctx, *history)
		if err != nil {
			return err
		}

		*history = append(*history, resp)
		if len(resp.ToolCalls) == 0 {
			if strings.TrimSpace(resp.Content) != "" {
				fmt.Println(resp.Content)
			}
			fmt.Println()
			return nil
		}

		toolResults := make([]*schema.Message, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			if tc.Function.Name != "bash" {
				toolResults = append(toolResults, schema.ToolMessage("Error: Unsupported tool", tc.ID, schema.WithToolName(tc.Function.Name)))
				continue
			}

			var in bashInput
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &in); err != nil {
				toolResults = append(toolResults, schema.ToolMessage("Error: Invalid tool arguments", tc.ID, schema.WithToolName(tc.Function.Name)))
				continue
			}

			fmt.Printf("\033[33m$ %s\033[0m\n", in.Command)
			out := runBash(in.Command)
			preview := out
			if len(preview) > 200 {
				preview = preview[:200]
			}
			fmt.Println(preview)

			toolResults = append(toolResults, schema.ToolMessage(out, tc.ID, schema.WithToolName(tc.Function.Name)))
		}

		*history = append(*history, toolResults...)
	}
}

func runBash(command string) string {
	for _, d := range dangerousPatterns {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-Command", command)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-lc", command)
	}

	wd, err := os.Getwd()
	if err == nil {
		cmd.Dir = wd
	}

	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "Error: Timeout (120s)"
	}

	result := strings.TrimSpace(string(out))
	if result == "" {
		result = "(no output)"
	}
	if err != nil && result == "" {
		result = err.Error()
	}
	if len(result) > maxToolOutput {
		result = result[:maxToolOutput]
	}

	return result
}
