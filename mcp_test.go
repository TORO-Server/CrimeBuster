package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMCP は Discord-MCP の代わりに、受けた tools/call を覚えて、決めた文章を返す。
type fakeMCP struct {
	calls []map[string]any
	reply func(tool string, args map[string]any) (string, bool)
	proto string
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.proto = r.Header.Get("Mcp-Protocol-Version")
	var req struct {
		ID     int64 `json:"id"`
		Method string
		Params struct {
			Name      string
			Arguments map[string]any
		}
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	args := req.Params.Arguments
	f.calls = append(f.calls, map[string]any{"name": req.Params.Name, "args": args})
	tool, _ := args["toolName"].(string)
	if tool == "" { // discord_search_tools のように、直接呼ぶツール
		tool = req.Params.Name
	}
	text, isErr := f.reply(tool, args)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}})
}

func TestReporterPostsThroughMCP(t *testing.T) {
	f := &fakeMCP{reply: func(tool string, args map[string]any) (string, bool) {
		switch tool {
		case "discord_create_thread":
			return `{"id":"777"}`, false
		}
		return `{"id":"1"}`, false
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	img := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(img, []byte("PNGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewReporter(NewMCP(srv.URL, "2025-11-25", true), "TOKEN", "100", "55")
	id, err := r.Post(context.Background(), "", "fuuty119", "本文", []ReportFile{{Name: "a.png", Path: img}}, true)
	if err != nil || id != "777" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if f.proto != "2025-11-25" {
		t.Fatalf("protocol header: %q", f.proto)
	}
	args := f.calls[0]["args"].(map[string]any)
	if f.calls[0]["name"] != "discord_call_tool" || args["toolName"] != "discord_create_thread" {
		t.Fatalf("call: %v", f.calls[0])
	}
	if args["authOverride"] != "Bot TOKEN" {
		t.Fatalf("authOverride: %v", args["authOverride"])
	}
	if args["pathParams"].(map[string]any)["channel_id"] != "100" {
		t.Fatalf("pathParams: %v", args["pathParams"])
	}
	body := args["body"].(map[string]any)
	if body["name"] != "fuuty119" || body["applied_tags"].([]any)[0] != "55" || body["message"].(map[string]any)["content"] != "本文" {
		t.Fatalf("body: %v", body)
	}
	assertNoMentions(t, body["message"].(map[string]any))
	file := args["files"].([]any)[0].(map[string]any)
	if b, _ := base64.StdEncoding.DecodeString(file["contentBase64"].(string)); string(b) != "PNGDATA" || file["filename"] != "a.png" {
		t.Fatalf("file: %v", file)
	}
}

func TestReporterAppendsToExistingThread(t *testing.T) {
	f := &fakeMCP{reply: func(tool string, args map[string]any) (string, bool) { return `{"id":"9"}`, false }}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := NewReporter(NewMCP(srv.URL, "2025-11-25", true), "T", "100", "55")
	id, err := r.Post(context.Background(), "888", "x", "続き", nil, true)
	if err != nil || id != "888" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls: %v", f.calls)
	}
	if f.calls[0]["args"].(map[string]any)["toolName"] != "discord_create_message" || f.calls[1]["args"].(map[string]any)["toolName"] != "discord_update_channel" {
		t.Fatalf("calls: %v", f.calls)
	}
	assertNoMentions(t, f.calls[0]["args"].(map[string]any)["body"].(map[string]any))
}

// assertNoMentions は、投稿の本文が、だれにも通知しない設定かを確かめる。
func assertNoMentions(t *testing.T, m map[string]any) {
	t.Helper()
	am, ok := m["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_mentions が無い: %v", m)
	}
	if parse, ok := am["parse"].([]any); !ok || len(parse) != 0 {
		t.Fatalf("allowed_mentions: %v", am)
	}
}

func TestReporterFallsBackToNewThread(t *testing.T) {
	f := &fakeMCP{reply: func(tool string, args map[string]any) (string, bool) {
		if tool == "discord_create_message" {
			return "HTTP 404 Not Found", true
		}
		return `{"id":"31"}`, false
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := NewReporter(NewMCP(srv.URL, "2025-11-25", true), "T", "100", "")
	id, err := r.Post(context.Background(), "888", "x", "y", nil, false)
	if err != nil || id != "31" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestMCPToolErrorIsReturned(t *testing.T) {
	f := &fakeMCP{reply: func(tool string, args map[string]any) (string, bool) {
		return "HTTP 403 Forbidden\n\n{\"message\": \"Missing Access\", \"code\": 50001}", true
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, err := NewMCP(srv.URL, "2025-11-25", true).Discord(context.Background(), DiscordCall{Tool: "discord_get_channel"})
	if err == nil || !strings.Contains(err.Error(), "Missing Access") {
		t.Fatalf("err=%v", err)
	}
}

func TestMCPDirectToolsWhenNotLazy(t *testing.T) {
	var gotName string
	var gotArgs map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Name      string
				Arguments map[string]any
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotName, gotArgs = req.Params.Name, req.Params.Arguments
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"id\":\"5\"}"}]}}`))
	}))
	defer srv.Close()
	out, err := NewMCP(srv.URL, "2025-11-25", false).Discord(context.Background(), DiscordCall{Tool: "discord_get_channel", PathParams: map[string]any{"channel_id": "1"}})
	if err != nil || out != `{"id":"5"}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if gotName != "discord_get_channel" || gotArgs["channel_id"] != "1" {
		t.Fatalf("name=%q args=%v", gotName, gotArgs)
	}
	// パスの値で、予約の引数 (トークンや添付) を上書きできない
	for _, reserved := range []string{"authOverride", "files", "body", "toolName"} {
		gotName = ""
		_, err := NewMCP(srv.URL, "2025-11-25", false).Discord(context.Background(), DiscordCall{
			Tool: "discord_get_channel", Auth: "Bot real", PathParams: map[string]any{reserved: "other"},
		})
		if err == nil || gotName != "" {
			t.Errorf("%s: err=%v name=%q", reserved, err, gotName)
		}
	}
}

func TestReadRPCEventStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n\n"))
	}))
	defer srv.Close()
	out, err := NewMCP(srv.URL, "2025-11-25", true).Tool(context.Background(), "x", nil)
	if err != nil || out != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
