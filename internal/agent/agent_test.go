package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The test binary re-runs itself as a fake agent CLI that speaks the real protocol, so adapters are tested without any AI service.
func TestMain(m *testing.M) {
	switch os.Getenv("FAKE_AGENT") {
	case "claude":
		runFakeClaude()
	case "codex":
		runFakeCodex()
	case "gemini":
		runFakeGemini()
	default:
		os.Exit(m.Run())
	}
	os.Exit(0)
}

func startFakeAgent(t *testing.T, agentName string) Conversation {
	t.Helper()
	t.Setenv("FAKE_AGENT", agentName)
	definition := Definition{
		command: []string{os.Args[0]},
		connect: definitions[agentName].connect,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conversation, err := definition.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conversation.Close() })
	return conversation
}

func askFake(t *testing.T, conversation Conversation, prompt string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return conversation.Ask(ctx, prompt)
}

func TestAgentConversationsKeepContextAcrossTurns(t *testing.T) {
	for _, agentName := range []string{"claude", "codex", "gemini"} {
		t.Run(agentName, func(t *testing.T) {
			conversation := startFakeAgent(t, agentName)

			first, err := askFake(t, conversation, "hello")
			require.NoError(t, err)
			second, err := askFake(t, conversation, "again")
			require.NoError(t, err)

			assert.Equal(t, "turn 1: hello", first)
			assert.Equal(t, "turn 2: again", second)
		})
	}
}

func TestAgentConversationsReportFailedTurn(t *testing.T) {
	for _, agentName := range []string{"claude", "codex", "gemini"} {
		t.Run(agentName, func(t *testing.T) {
			conversation := startFakeAgent(t, agentName)

			_, err := askFake(t, conversation, "fail")

			require.Error(t, err)
			assert.Contains(t, err.Error(), "boom")
		})
	}
}

func TestAgentConversationReportsExitedProcessWithStderr(t *testing.T) {
	conversation := startFakeAgent(t, "claude")

	_, err := askFake(t, conversation, "exit")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "fake agent crashed")
}

func TestAgentConversationHonorsContextCancel(t *testing.T) {
	conversation := startFakeAgent(t, "claude")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := conversation.Ask(ctx, "hang")

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestLookup(t *testing.T) {
	definition, err := Lookup("claude")
	require.NoError(t, err)
	assert.Equal(t, "claude", definition.command[0])

	_, err = Lookup("")
	assert.Error(t, err)
	_, err = Lookup("nope")
	assert.Error(t, err)
}

// --- fake agent CLIs ---

type fakeIO struct {
	in *bufio.Scanner
}

func newFakeIO() *fakeIO {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	return &fakeIO{in: in}
}

func (f *fakeIO) read() map[string]any {
	if !f.in.Scan() {
		os.Exit(0)
	}
	var msg map[string]any
	if err := json.Unmarshal(f.in.Bytes(), &msg); err != nil {
		fakeCrash("invalid json from client: " + err.Error())
	}
	return msg
}

func (f *fakeIO) write(msg any) {
	line, _ := json.Marshal(msg)
	fmt.Println(string(line))
}

func fakeCrash(reason string) {
	fmt.Fprintln(os.Stderr, reason)
	os.Exit(1)
}

func fakeReply(turn int, prompt string) string {
	return fmt.Sprintf("turn %d: %s", turn, prompt)
}

func runFakeClaude() {
	f := newFakeIO()
	for turn := 1; ; turn++ {
		msg := f.read()
		prompt := msg["message"].(map[string]any)["content"].(string)
		switch prompt {
		case "exit":
			fakeCrash("fake agent crashed")
		case "hang":
			select {}
		}

		f.write(map[string]any{"type": "system", "subtype": "init"})
		f.write(map[string]any{"type": "assistant", "message": map[string]any{"content": "thinking"}})
		if prompt == "fail" {
			f.write(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "boom"})
			continue
		}
		f.write(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fakeReply(turn, prompt)})
	}
}

func runFakeCodex() {
	f := newFakeIO()
	turn := 0
	for {
		msg := f.read()
		id := msg["id"]
		switch msg["method"] {
		case "initialize":
			f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"userAgent": "fake"}})
		case "initialized":
		case "thread/start":
			f.write(map[string]any{"jsonrpc": "2.0", "method": "thread/started", "params": map[string]any{}})
			f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}})
		case "turn/start":
			turn++
			params := msg["params"].(map[string]any)
			if params["threadId"] != "thread-1" {
				fakeCrash("unexpected threadId")
			}
			prompt := params["input"].([]any)[0].(map[string]any)["text"].(string)
			f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"turn": map[string]any{"id": "turn"}}})
			f.write(map[string]any{"jsonrpc": "2.0", "method": "item/completed", "params": map[string]any{"item": map[string]any{"type": "reasoning"}}})
			if prompt == "fail" {
				f.write(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{
					"turn": map[string]any{"status": "failed", "error": map[string]any{"message": "boom"}},
				}})
				continue
			}
			f.write(map[string]any{"jsonrpc": "2.0", "method": "item/completed", "params": map[string]any{"item": map[string]any{"type": "agentMessage", "text": fakeReply(turn, prompt)}}})
			f.write(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{"turn": map[string]any{"status": "completed"}}})
		}
	}
}

func runFakeGemini() {
	f := newFakeIO()
	turn := 0
	for {
		msg := f.read()
		id := msg["id"]
		switch msg["method"] {
		case "initialize":
			f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": 1}})
		case "session/new":
			f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"sessionId": "session-1"}})
		case "session/prompt":
			turn++
			params := msg["params"].(map[string]any)
			if params["sessionId"] != "session-1" {
				fakeCrash("unexpected sessionId")
			}
			prompt := params["prompt"].([]any)[0].(map[string]any)["text"].(string)

			// The agent asks the client for permission; the client must decline without stalling the turn.
			f.write(map[string]any{"jsonrpc": "2.0", "id": "perm-1", "method": "session/request_permission", "params": map[string]any{}})
			permission := f.read()
			outcome := permission["result"].(map[string]any)["outcome"].(map[string]any)["outcome"]
			if permission["id"] != "perm-1" || outcome != "cancelled" {
				fakeCrash("permission request was not cancelled")
			}

			if prompt == "fail" {
				f.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": 400, "message": "boom"}})
				continue
			}
			sendChunk := func(kind string, text string) {
				f.write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "session-1",
					"update":    map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}},
				}})
			}
			sendChunk("agent_thought_chunk", "hidden reasoning")
			sendChunk("agent_message_chunk", fmt.Sprintf("turn %d: ", turn))
			sendChunk("agent_message_chunk", prompt)
			f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
		}
	}
}
