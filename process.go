package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxStderrBytes = 64 << 10

// lineProcess is a long-running agent CLI that exchanges newline-delimited JSON over stdin and stdout.
type lineProcess struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	lines     chan []byte
	stopped   chan struct{}
	stopOnce  sync.Once
	stderrLog *cappedBuffer
}

func startLineProcess(command []string) (*lineProcess, error) {
	cmd := exec.Command(command[0], command[1:]...)
	// Own process group so Close also kills helpers the CLI spawns (MCP servers, shells).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	stderrLog := &cappedBuffer{limit: maxStderrBytes}
	cmd.Stderr = stderrLog

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command[0], err)
	}

	process := &lineProcess{
		cmd:       cmd,
		stdin:     stdin,
		lines:     make(chan []byte),
		stopped:   make(chan struct{}),
		stderrLog: stderrLog,
	}
	go process.readLines(stdout)
	return process, nil
}

func (p *lineProcess) readLines(stdout io.Reader) {
	defer close(p.lines)
	reader := bufio.NewReader(stdout)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			select {
			case p.lines <- line:
			case <-p.stopped:
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}

func (p *lineProcess) send(message any) error {
	line, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = p.stdin.Write(append(line, '\n'))
	return err
}

func (p *lineProcess) next(ctx context.Context) ([]byte, error) {
	select {
	case line, isOpen := <-p.lines:
		if !isOpen {
			return nil, fmt.Errorf("agent process exited: %s", strings.TrimSpace(p.stderrLog.String()))
		}
		return line, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *lineProcess) Close() error {
	p.stopOnce.Do(func() {
		close(p.stopped)
		_ = p.stdin.Close()
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		_ = p.cmd.Wait()
	})
	return nil
}

// cappedBuffer keeps the first bytes of stderr for error messages without growing for the whole session.
type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.limit - b.buf.Len()
	if room > 0 {
		b.buf.Write(data[:min(room, len(data))])
	}
	return len(data), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rpcConn speaks JSON-RPC 2.0 over a lineProcess; codex app-server and gemini ACP both use it.
type rpcConn struct {
	process *lineProcess
	lastID  int
	// answerAgentRequest returns the reply to a request the agent sends us; ok=false replies "method not found".
	answerAgentRequest func(method string) (result any, ok bool)
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *rpcConn) notify(method string, params any) error {
	return c.process.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// call sends a request and returns its result, passing notifications that arrive meanwhile to onNotification.
func (c *rpcConn) call(ctx context.Context, method string, params any, onNotification func(rpcMessage)) (json.RawMessage, error) {
	c.lastID++
	requestID := c.lastID
	err := c.process.send(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": method, "params": params})
	if err != nil {
		return nil, err
	}

	var result json.RawMessage
	err = c.readUntil(ctx, func(message rpcMessage) (bool, error) {
		if message.Method != "" {
			if onNotification != nil {
				onNotification(message)
			}
			return false, nil
		}
		if string(message.ID) != fmt.Sprint(requestID) {
			return false, nil
		}
		if message.Error != nil {
			return true, fmt.Errorf("%s failed: %s", method, message.Error.Message)
		}
		result = message.Result
		return true, nil
	})
	return result, err
}

// readUntil feeds incoming notifications and responses to handle until it reports done; agent requests are answered here.
func (c *rpcConn) readUntil(ctx context.Context, handle func(rpcMessage) (done bool, err error)) error {
	for {
		line, err := c.process.next(ctx)
		if err != nil {
			return err
		}
		var message rpcMessage
		if err := json.Unmarshal(line, &message); err != nil {
			continue // CLIs occasionally print non-protocol lines to stdout
		}

		isAgentRequest := message.Method != "" && len(message.ID) > 0
		if isAgentRequest {
			if err := c.replyToAgent(message); err != nil {
				return err
			}
			continue
		}

		done, err := handle(message)
		if done || err != nil {
			return err
		}
	}
}

func (c *rpcConn) replyToAgent(request rpcMessage) error {
	reply := map[string]any{"jsonrpc": "2.0", "id": request.ID}
	result, ok := c.answerAgentRequest(request.Method)
	if ok {
		reply["result"] = result
	} else {
		reply["error"] = rpcError{Code: -32601, Message: "not supported by send-to-my-agent"}
	}
	return c.process.send(reply)
}
