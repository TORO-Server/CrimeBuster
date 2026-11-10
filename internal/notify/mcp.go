package notify

import (
	"bufio"
	"bytes"
	"context"
	"crime-buster/internal/domain"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// MCP は Streamable HTTP の MCP サーバー (Discord-MCP) を呼ぶ最小のクライアント。
// 状態を持たない (ステートレス) 方式なので、initialize もセッションも要らず、tools/call を 1 回 POST するだけで済む。
type MCP struct {
	URL      string
	Protocol string // Mcp-Protocol-Version に入れる値
	Lazy     bool   // サーバーが discord_call_tool 1 本にまとめているか (DISCORD_MCP_LAZY_TOOLS=true)
	hc       *http.Client
	seq      atomic.Int64
}

func NewMCP(url, protocol string, lazy bool) *MCP {
	return &MCP{URL: url, Protocol: protocol, Lazy: lazy, hc: &http.Client{Timeout: 3 * time.Minute}}
}

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResp struct {
	Result *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Tool は MCP のツールを呼んで、返ってきた文章を返す。ツールがエラーを返したときも error にする。
func (m *MCP) Tool(ctx context.Context, name string, args map[string]any) (string, error) {
	body, err := json.Marshal(rpcReq{JSONRPC: "2.0", ID: m.seq.Add(1), Method: "tools/call", Params: map[string]any{"name": name, "arguments": args}})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", m.Protocol)
	resp, err := m.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("mcp %s: %w", name, err)
	}
	defer resp.Body.Close()
	raw, err := readRPC(resp)
	if err != nil {
		return "", fmt.Errorf("mcp %s: %w", name, err)
	}
	var r rpcResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("mcp %s: bad response: %w", name, err)
	}
	if r.Error != nil {
		return "", fmt.Errorf("mcp %s: %s (%d)", name, r.Error.Message, r.Error.Code)
	}
	if r.Result == nil {
		return "", fmt.Errorf("mcp %s: empty result (http %d)", name, resp.StatusCode)
	}
	var sb strings.Builder
	for _, c := range r.Result.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if r.Result.IsError {
		return "", fmt.Errorf("mcp %s: %s", name, domain.OneLine(sb.String(), 400))
	}
	return sb.String(), nil
}

// readRPC は、JSON の応答も、SSE (data: の行) の応答も受ける。
func readRPC(resp *http.Response) ([]byte, error) {
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		var last []byte
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
				last = []byte(strings.TrimSpace(d))
			}
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
		if last == nil {
			return nil, fmt.Errorf("no data in event stream (http %d)", resp.StatusCode)
		}
		return last, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 500 || len(b) == 0 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, domain.OneLine(string(b), 200))
	}
	return b, nil
}

// DiscordCall は Discord の REST を 1 つ呼ぶ。
type DiscordCall struct {
	Tool       string         // 例: discord_create_message
	PathParams map[string]any // 例: {"channel_id": "..."}
	Body       map[string]any
	Query      map[string]any   // URL の ?name=value (例: {"limit": 20})
	Files      []map[string]any // {filename, contentType, contentBase64}
	Fields     string           // 返事から取り出す項目 (トークンの節約)
	Auth       string           // Authorization ヘッダの値 ("Bot ...")。サーバーのトークンの代わりに使う
	Reason     string
}

// Discord は MCP 経由で Discord の REST を呼ぶ。
func (m *MCP) Discord(ctx context.Context, c DiscordCall) (string, error) {
	args := map[string]any{}
	if len(c.PathParams) > 0 {
		args["pathParams"] = c.PathParams
	}
	if c.Body != nil {
		args["body"] = c.Body
	}
	if len(c.Query) > 0 {
		args["queryParams"] = c.Query
	}
	if len(c.Files) > 0 {
		args["files"] = c.Files
	}
	if c.Fields != "" {
		args["fields"] = c.Fields
	}
	if c.Auth != "" {
		args["authOverride"] = c.Auth
	}
	if c.Reason != "" {
		args["auditLogReason"] = c.Reason
	}
	if m.Lazy {
		args["toolName"] = c.Tool
		return m.Tool(ctx, "discord_call_tool", args)
	}
	// 個別のツールとして登録されているときは、パスの値を引数に平らにする。予約の引数 (トークンや添付) は上書きさせない
	for k, v := range c.PathParams {
		if mcpReservedArgs[k] {
			return "", fmt.Errorf("mcp %s: パスの値に %q は使えません", c.Tool, k)
		}
		args[k] = v
	}
	delete(args, "pathParams")
	return m.Tool(ctx, c.Tool, args)
}

// mcpReservedArgs は、Discord が組み立てる引数の名前。
var mcpReservedArgs = map[string]bool{
	"pathParams": true, "body": true, "queryParams": true, "files": true, "fields": true,
	"authOverride": true, "auditLogReason": true, "toolName": true,
}
