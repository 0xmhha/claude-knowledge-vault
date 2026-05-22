package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// echoTool is a minimal Tool implementation for testing — echoes back the
// "msg" argument verbatim. If "fail":true is in the args, returns isError.
type echoTool struct {
	called int
	mu     sync.Mutex
}

func (e *echoTool) Name() string        { return "echo" }
func (e *echoTool) Description() string { return "Echoes back the msg argument." }
func (e *echoTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"},"fail":{"type":"boolean"}}}`)
}
func (e *echoTool) Call(_ context.Context, raw json.RawMessage) (output string, isError bool, err error) {
	var args struct {
		Msg  string `json:"msg"`
		Fail bool   `json:"fail"`
		Boom bool   `json:"boom"`
	}
	if err := UnmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	e.mu.Lock()
	e.called++
	e.mu.Unlock()
	if args.Boom {
		return "", false, errors.New("boom")
	}
	return args.Msg, args.Fail, nil
}

// runServer pipes scripted input through Server.Run and collects the
// response lines. Returns one response per line (filtered for non-empty).
func runServer(t *testing.T, s *Server, input string) []string {
	t.Helper()
	in := strings.NewReader(input)
	var out bytes.Buffer
	if err := s.Run(context.Background(), in, &out); err != nil && err != io.EOF {
		t.Fatalf("Run: %v", err)
	}
	raw := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	lines := raw[:0]
	for _, l := range raw {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// parseEnvelope unmarshals one JSON-RPC response line.
func parseEnvelope(t *testing.T, line string) response {
	t.Helper()
	var env response
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("response not JSON: %v\nline: %s", err, line)
	}
	return env
}

func TestInitialize(t *testing.T) {
	s := New("test-server", "0.1.0")
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	out := runServer(t, s, in)
	if len(out) != 1 {
		t.Fatalf("expected 1 response, got %d:\n%s", len(out), strings.Join(out, "\n"))
	}
	env := parseEnvelope(t, out[0])
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	resultBytes, _ := json.Marshal(env.Result)
	resultStr := string(resultBytes)
	if !strings.Contains(resultStr, ProtocolVersion) {
		t.Errorf("missing protocolVersion in result: %s", resultStr)
	}
	if !strings.Contains(resultStr, "test-server") {
		t.Errorf("missing serverInfo.name: %s", resultStr)
	}
	if !strings.Contains(resultStr, `"tools":`) {
		t.Errorf("missing tools capability: %s", resultStr)
	}
}

func TestNotificationsHaveNoResponse(t *testing.T) {
	s := New("t", "v")
	// No "id" field → notification per JSON-RPC 2.0.
	in := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n"
	out := runServer(t, s, in)
	if len(out) != 0 {
		t.Errorf("notification triggered a response: %v", out)
	}
}

func TestToolsList(t *testing.T) {
	s := New("t", "v")
	s.RegisterTool(&echoTool{})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	out := runServer(t, s, in)
	if len(out) != 1 {
		t.Fatalf("expected 1 response, got %d", len(out))
	}
	env := parseEnvelope(t, out[0])
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	resBytes, _ := json.Marshal(env.Result)
	if !strings.Contains(string(resBytes), `"name":"echo"`) {
		t.Errorf("tools list missing echo: %s", resBytes)
	}
}

func TestToolsCall_Success(t *testing.T) {
	s := New("t", "v")
	echo := &echoTool{}
	s.RegisterTool(echo)
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hi"}}}` + "\n"
	out := runServer(t, s, in)
	env := parseEnvelope(t, out[0])
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	resBytes, _ := json.Marshal(env.Result)
	if !strings.Contains(string(resBytes), `"text":"hi"`) {
		t.Errorf("expected echoed text, got: %s", resBytes)
	}
	echo.mu.Lock()
	defer echo.mu.Unlock()
	if echo.called != 1 {
		t.Errorf("expected tool called once, got %d", echo.called)
	}
}

func TestToolsCall_IsError(t *testing.T) {
	s := New("t", "v")
	s.RegisterTool(&echoTool{})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"oops","fail":true}}}` + "\n"
	out := runServer(t, s, in)
	env := parseEnvelope(t, out[0])
	if env.Error != nil {
		t.Fatalf("isError is a tool-level signal, not JSON-RPC error: %+v", env.Error)
	}
	resBytes, _ := json.Marshal(env.Result)
	if !strings.Contains(string(resBytes), `"isError":true`) {
		t.Errorf("missing isError flag: %s", resBytes)
	}
}

func TestToolsCall_UnknownTool(t *testing.T) {
	s := New("t", "v")
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"missing"}}` + "\n"
	out := runServer(t, s, in)
	env := parseEnvelope(t, out[0])
	if env.Error == nil {
		t.Fatalf("expected JSON-RPC error for unknown tool, got: %+v", env)
	}
	if env.Error.Code != codeMethodNotFound {
		t.Errorf("expected code %d, got %d", codeMethodNotFound, env.Error.Code)
	}
}

func TestToolsCall_InternalErrorWrapsToRPC(t *testing.T) {
	s := New("t", "v")
	s.RegisterTool(&echoTool{})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"boom":true}}}` + "\n"
	out := runServer(t, s, in)
	env := parseEnvelope(t, out[0])
	if env.Error == nil {
		t.Fatalf("expected JSON-RPC error for tool internal failure")
	}
	if env.Error.Code != codeInternalError {
		t.Errorf("expected code %d, got %d", codeInternalError, env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "boom") {
		t.Errorf("error message lost: %q", env.Error.Message)
	}
}

func TestUnknownMethod(t *testing.T) {
	s := New("t", "v")
	in := `{"jsonrpc":"2.0","id":1,"method":"definitely/notreal"}` + "\n"
	out := runServer(t, s, in)
	env := parseEnvelope(t, out[0])
	if env.Error == nil || env.Error.Code != codeMethodNotFound {
		t.Errorf("expected MethodNotFound, got: %+v", env)
	}
}

func TestParseError(t *testing.T) {
	s := New("t", "v")
	out := runServer(t, s, "this is not json\n")
	env := parseEnvelope(t, out[0])
	if env.Error == nil || env.Error.Code != codeParseError {
		t.Errorf("expected ParseError, got: %+v", env)
	}
}

func TestWrongJSONRPCVersion(t *testing.T) {
	s := New("t", "v")
	in := `{"jsonrpc":"1.0","id":1,"method":"initialize"}` + "\n"
	out := runServer(t, s, in)
	env := parseEnvelope(t, out[0])
	if env.Error == nil || env.Error.Code != codeInvalidRequest {
		t.Errorf("expected InvalidRequest for non-2.0 jsonrpc, got: %+v", env)
	}
}

func TestPing(t *testing.T) {
	s := New("t", "v")
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	out := runServer(t, s, in)
	env := parseEnvelope(t, out[0])
	if env.Error != nil {
		t.Errorf("ping should succeed: %+v", env.Error)
	}
}

func TestSequentialMessages(t *testing.T) {
	s := New("t", "v")
	s.RegisterTool(&echoTool{})
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"abc"}}}` + "\n"
	out := runServer(t, s, in)
	// 3 responses (notification consumed silently)
	if len(out) != 3 {
		t.Fatalf("expected 3 responses, got %d:\n%s", len(out), strings.Join(out, "\n"))
	}
}

func TestRegisterTool_Replaces(t *testing.T) {
	s := New("t", "v")
	s.RegisterTool(&echoTool{})
	// Same name → replaces; tools/list should still report one entry.
	s.RegisterTool(&echoTool{})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	out := runServer(t, s, in)
	env := parseEnvelope(t, out[0])
	resBytes, _ := json.Marshal(env.Result)
	if strings.Count(string(resBytes), `"name":"echo"`) != 1 {
		t.Errorf("expected exactly one echo entry, got: %s", resBytes)
	}
}

func TestUnmarshalArgs_EmptyAndNull(t *testing.T) {
	var dst struct{ X int }
	if err := UnmarshalArgs(nil, &dst); err != nil {
		t.Errorf("nil should be no-op, got: %v", err)
	}
	if err := UnmarshalArgs(json.RawMessage("null"), &dst); err != nil {
		t.Errorf("null should be no-op, got: %v", err)
	}
	if err := UnmarshalArgs(json.RawMessage(`{"x":42}`), &dst); err != nil {
		t.Errorf("valid args: %v", err)
	}
	if dst.X != 42 {
		t.Errorf("X = %d, want 42", dst.X)
	}
}

func TestUnmarshalArgs_Invalid(t *testing.T) {
	var dst struct{ X int }
	if err := UnmarshalArgs(json.RawMessage("not-json"), &dst); err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}
