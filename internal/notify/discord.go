package notify

import (
	"context"
	"crime-buster/internal/config"
	"crime-buster/internal/domain"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"strings"
)

// LoadBotToken は DiscordSRV の config.yml の BotToken を読む。
// このボットは DiscordSRV のものを借りる (トークンを .env やリポジトリに写さない。config.yml を読み取り専用でマウントする)。
func LoadBotToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var y struct {
		BotToken string `yaml:"BotToken"`
	}
	if err := yaml.Unmarshal(b, &y); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	t := strings.TrimSpace(y.BotToken)
	if t == "" || strings.EqualFold(t, "BOTTOKEN") {
		return "", fmt.Errorf("BotToken is empty in %s", path)
	}
	return t, nil
}

// Reporter は通報チャンネル (フォーラム) に投稿する。Discord の REST は、Discord-MCP (MCP サーバー) の経由で呼ぶ。
// Gateway には接続しない (DiscordSRV が同じトークンで Gateway につないでいるので、もう 1 本つながない)。
// Discord-MCP は自分のトークンを持たなくてよい。DiscordSRV のトークンを、呼ぶたびに authOverride で渡す
// (サーバー側で DISCORD_MCP_ALLOW_AUTH_OVERRIDE=true にしておく)。
type Reporter struct {
	mcp       *MCP
	auth      string // "Bot <token>"
	channelID string
	tagID     string
}

func NewReporter(mcp *MCP, token, channelID, rollbackTagID string) *Reporter {
	return &Reporter{mcp: mcp, auth: "Bot " + token, channelID: channelID, tagID: rollbackTagID}
}

// reporterFor は設定から、ボットのトークンと Discord-MCP を使う Reporter を作る。
// DISCORD_BOT_TOKEN (または _FILE) があれば、それを使う。無ければ DiscordSRV の config.yml から読む。
func ReporterFor(cfg *config.Config) (*Reporter, error) {
	tok := cfg.DiscordConfig.BotToken
	if tok == "" {
		var err error
		if tok, err = LoadBotToken(cfg.DiscordConfig.DiscordSRVConfig); err != nil {
			return nil, fmt.Errorf("discordsrv token: %w", err)
		}
	}
	return NewReporter(NewMCP(cfg.DiscordConfig.DiscordMCPURL, cfg.DiscordConfig.MCPProtocol, cfg.DiscordConfig.DiscordMCPLazy), tok, cfg.DiscordConfig.ReportChannelID, cfg.DiscordConfig.RollbackTagID), nil
}

// noMentions は、投稿の allowed_mentions。文字に @everyone などがあっても、だれにも通知しない。
func NoMentions() map[string]any { return map[string]any{"parse": []string{}} }

// Check は投稿せずに、MCP とトークンとチャンネルが使えるか確かめる。
func (r *Reporter) Check(ctx context.Context) (string, error) {
	if r.channelID == "" {
		return "", fmt.Errorf("REPORT_CHANNEL_ID is not set")
	}
	out, err := r.mcp.Discord(ctx, DiscordCall{Tool: "discord_get_my_user", Fields: "id,username", Auth: r.auth})
	if err != nil {
		return "", fmt.Errorf("users/@me: %w", err)
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal([]byte(out), &me); err != nil {
		return "", fmt.Errorf("users/@me: bad response: %s", domain.OneLine(out, 120))
	}
	out, err = r.mcp.Discord(ctx, DiscordCall{Tool: "discord_get_channel", PathParams: map[string]any{"channel_id": r.channelID}, Fields: "id,name,type", Auth: r.auth})
	if err != nil {
		return "", fmt.Errorf("channel %s: %w", r.channelID, err)
	}
	var ch struct {
		Name string `json:"name"`
		Type int    `json:"type"`
	}
	if err := json.Unmarshal([]byte(out), &ch); err != nil {
		return "", fmt.Errorf("channel %s: bad response: %s", r.channelID, domain.OneLine(out, 120))
	}
	if ch.Type != 15 { // GUILD_FORUM
		return "", fmt.Errorf("channel %s is not a forum (type %d)", r.channelID, ch.Type)
	}
	return fmt.Sprintf("bot=%s channel=%s (forum) via MCP", me.Username, ch.Name), nil
}

type ReportFile struct {
	Name string
	Path string
}

// mcpFiles は画像を Base64 にして、MCP に渡す形にする (サーバーと同じディスクを共有しなくて済む)。
func mcpFiles(files []ReportFile) ([]map[string]any, error) {
	var out []map[string]any
	for _, f := range files {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"filename": f.Name, "contentType": "image/png", "contentBase64": base64.StdEncoding.EncodeToString(b)})
	}
	return out, nil
}

// parseID は、Discord の返事 (JSON) から id を取り出す。
func parseID(s string) (string, error) {
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil || v.ID == "" {
		return "", fmt.Errorf("no id in response: %s", domain.OneLine(s, 120))
	}
	return v.ID, nil
}

// Post は新しい投稿を立てる。existing があれば、その投稿に書き足す。投稿の ID を返す。
// rolledBack のときは、投稿に「ロルバ済」のタグを付ける。
func (r *Reporter) Post(ctx context.Context, existing, title, content string, files []ReportFile, rolledBack bool) (string, error) {
	var tags []string
	if rolledBack && r.tagID != "" {
		tags = append(tags, r.tagID)
	}
	return r.PostTags(ctx, existing, title, content, files, tags)
}

// PostTags は Post と同じで、付けるタグを (ID で) 指定する。
func (r *Reporter) PostTags(ctx context.Context, existing, title, content string, files []ReportFile, tags []string) (string, error) {
	if existing == "" && r.channelID == "" {
		return "", fmt.Errorf("REPORT_CHANNEL_ID is not set")
	}
	mf, err := mcpFiles(files)
	if err != nil {
		return "", err
	}

	if existing != "" {
		_, err := r.mcp.Discord(ctx, DiscordCall{
			Tool: "discord_create_message", PathParams: map[string]any{"channel_id": existing},
			Body: map[string]any{"content": content, "allowed_mentions": NoMentions()}, Files: mf, Fields: "id", Auth: r.auth,
		})
		if err == nil {
			if len(tags) > 0 {
				_, _ = r.mcp.Discord(ctx, DiscordCall{
					Tool: "discord_update_channel", PathParams: map[string]any{"channel_id": existing},
					Body: map[string]any{"applied_tags": tags}, Fields: "id", Auth: r.auth,
				})
			}
			return existing, nil
		}
		// 投稿が消えている・書けないときは、新しく立てる
	}
	out, err := r.mcp.Discord(ctx, DiscordCall{
		Tool: "discord_create_thread", PathParams: map[string]any{"channel_id": r.channelID},
		Body: map[string]any{
			"name":                  title,
			"auto_archive_duration": 10080,
			"applied_tags":          tags,
			"message":               map[string]any{"content": content, "allowed_mentions": NoMentions()},
		},
		Files: mf, Fields: "id", Auth: r.auth,
	})
	if err != nil {
		return "", err
	}
	return parseID(out)
}

// MCP は、Discord の操作に使う MCP の部品を返す。
func (r *Reporter) MCP() *MCP { return r.mcp }

// Auth は、Discord の API に渡す認可の値を返す。
func (r *Reporter) Auth() string { return r.auth }

// TagID は、通報に付けるタグの ID を返す。
func (r *Reporter) TagID() string { return r.tagID }

// ChannelID は、通報先のチャンネルの ID を返す。
func (r *Reporter) ChannelID() string { return r.channelID }
