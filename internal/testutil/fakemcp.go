package testutil

import (
	"encoding/json"
	"net/http"
)

// fakeMCP は Discord-MCP の代わりに、受けた tools/call を覚えて、決めた文章を返す。
type FakeMCP struct {
	Calls []map[string]any
	Reply func(tool string, args map[string]any) (string, bool)
	Proto string
}

func (f *FakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.Proto = r.Header.Get("Mcp-Protocol-Version")
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
	f.Calls = append(f.Calls, map[string]any{"name": req.Params.Name, "args": args})
	tool, _ := args["toolName"].(string)
	if tool == "" { // discord_search_tools のように、直接呼ぶツール
		tool = req.Params.Name
	}
	text, isErr := f.Reply(tool, args)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}})
}
