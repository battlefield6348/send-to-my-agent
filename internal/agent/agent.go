package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Conversation is one live agent process holding one conversation; Ask is called one turn at a time.
type Conversation interface {
	Ask(ctx context.Context, prompt string) (string, error)
	Close() error
}

type Definition struct {
	command []string
	connect func(ctx context.Context, process *lineProcess) (Conversation, error)
}

var definitions = map[string]Definition{
	"claude": {
		command: []string{"claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"},
		connect: connectClaude,
	},
	"codex": {
		command: []string{"codex", "app-server"},
		connect: connectCodex,
	},
	"gemini": {
		command: []string{"gemini", "--acp", "--skip-trust"},
		connect: connectGemini,
	},
}

func Lookup(agentName string) (Definition, error) {
	definition, isKnownAgent := definitions[agentName]
	if !isKnownAgent {
		return Definition{}, fmt.Errorf("AGENT must be one of claude, codex, gemini; got %q", agentName)
	}
	return definition, nil
}

func (d Definition) Start(ctx context.Context) (Conversation, error) {
	process, err := startLineProcess(d.command)
	if err != nil {
		return nil, err
	}
	conversation, err := d.connect(ctx, process)
	if err != nil {
		_ = process.Close()
		return nil, err
	}
	return conversation, nil
}

// claude: `claude -p --input-format stream-json` keeps reading user messages from stdin until stdin closes.
type claudeConversation struct {
	*lineProcess
}

func connectClaude(_ context.Context, process *lineProcess) (Conversation, error) {
	return claudeConversation{process}, nil
}

func (c claudeConversation) Ask(ctx context.Context, prompt string) (string, error) {
	userMessage := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": prompt},
	}
	if err := c.send(userMessage); err != nil {
		return "", err
	}

	for {
		line, err := c.next(ctx)
		if err != nil {
			return "", err
		}
		var event struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
		}
		if json.Unmarshal(line, &event) != nil || event.Type != "result" {
			continue
		}
		if event.IsError {
			return "", fmt.Errorf("claude turn failed (%s): %s", event.Subtype, event.Result)
		}
		return event.Result, nil
	}
}

// codex: `codex app-server` speaks JSON-RPC; one thread holds the conversation and each prompt is a turn.
type codexConversation struct {
	rpc      *rpcConn
	threadID string
}

func connectCodex(ctx context.Context, process *lineProcess) (Conversation, error) {
	rpc := &rpcConn{process: process, answerAgentRequest: declineAgentRequest}

	clientInfo := map[string]any{"name": "send-to-my-agent", "version": "0.1.0"}
	if _, err := rpc.call(ctx, "initialize", map[string]any{"clientInfo": clientInfo}, nil); err != nil {
		return nil, err
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}

	result, err := rpc.call(ctx, "thread/start", map[string]any{"approvalPolicy": "never"}, nil)
	if err != nil {
		return nil, err
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &started); err != nil {
		return nil, fmt.Errorf("codex thread/start: %w", err)
	}
	return &codexConversation{rpc: rpc, threadID: started.Thread.ID}, nil
}

func (c *codexConversation) Ask(ctx context.Context, prompt string) (string, error) {
	var answer string
	var turnDone bool
	var turnErr error
	trackTurn := func(message rpcMessage) {
		switch message.Method {
		case "item/completed":
			var completed struct {
				Item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(message.Params, &completed) == nil && completed.Item.Type == "agentMessage" {
				answer = completed.Item.Text
			}
		case "turn/completed":
			var completed struct {
				Turn struct {
					Status string `json:"status"`
					Error  *struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"turn"`
			}
			turnDone = true
			if json.Unmarshal(message.Params, &completed) != nil {
				turnErr = fmt.Errorf("codex turn/completed: unreadable params")
				return
			}
			if completed.Turn.Status != "completed" {
				turnErr = fmt.Errorf("codex turn %s", completed.Turn.Status)
				if completed.Turn.Error != nil {
					turnErr = fmt.Errorf("%w: %s", turnErr, completed.Turn.Error.Message)
				}
			}
		}
	}

	turnInput := []map[string]any{{"type": "text", "text": prompt}}
	_, err := c.rpc.call(ctx, "turn/start", map[string]any{"threadId": c.threadID, "input": turnInput}, trackTurn)
	if err != nil {
		return "", err
	}
	if !turnDone {
		err = c.rpc.readUntil(ctx, func(message rpcMessage) (bool, error) {
			trackTurn(message)
			return turnDone, nil
		})
	}
	if err != nil {
		return "", err
	}
	return answer, turnErr
}

func (c *codexConversation) Close() error {
	return c.rpc.process.Close()
}

// gemini: `gemini --acp` speaks the Agent Client Protocol; the reply streams in as agent_message_chunk updates.
type geminiConversation struct {
	rpc       *rpcConn
	sessionID string
}

func connectGemini(ctx context.Context, process *lineProcess) (Conversation, error) {
	rpc := &rpcConn{process: process, answerAgentRequest: answerGeminiRequest}

	// Advertising no fs capability makes gemini read files itself instead of asking us.
	clientCapabilities := map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}}
	_, err := rpc.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": clientCapabilities}, nil)
	if err != nil {
		return nil, err
	}

	workingDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	result, err := rpc.call(ctx, "session/new", map[string]any{"cwd": workingDir, "mcpServers": []any{}}, nil)
	if err != nil {
		return nil, err
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(result, &created); err != nil {
		return nil, fmt.Errorf("gemini session/new: %w", err)
	}
	return &geminiConversation{rpc: rpc, sessionID: created.SessionID}, nil
}

func (c *geminiConversation) Ask(ctx context.Context, prompt string) (string, error) {
	var answer strings.Builder
	collectMessageChunks := func(message rpcMessage) {
		if message.Method != "session/update" {
			return
		}
		var update struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		isMessageText := json.Unmarshal(message.Params, &update) == nil &&
			update.Update.SessionUpdate == "agent_message_chunk" &&
			update.Update.Content.Type == "text"
		if isMessageText {
			answer.WriteString(update.Update.Content.Text)
		}
	}

	promptBlocks := []map[string]any{{"type": "text", "text": prompt}}
	_, err := c.rpc.call(ctx, "session/prompt", map[string]any{"sessionId": c.sessionID, "prompt": promptBlocks}, collectMessageChunks)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(answer.String()), nil
}

func (c *geminiConversation) Close() error {
	return c.rpc.process.Close()
}

// Tool approvals are declined, matching the one-shot CLI default where unapproved tools are refused.
func answerGeminiRequest(method string) (any, bool) {
	if method == "session/request_permission" {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, true
	}
	return nil, false
}

func declineAgentRequest(string) (any, bool) {
	return nil, false
}
