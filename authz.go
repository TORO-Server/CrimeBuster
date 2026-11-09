package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// コネクタの認証。共有の秘密は使わず、利用者の OAuth 2.0 のトークン (Bearer) を、auth の introspection に尋ねて確かめる。
//
//	POST {AUTH_INTERNAL_URL}/introspect {"token"} → {"active": true, "claims": {id, username, roles, roles_source, ...}}
//
// auth は、claims.roles を、トークンの値ではなく「いまの Discord のロール」にして返す (roles_source=live)。
// OAuth のアクセストークンにはロールが入っていないし、90 日トークンのロールは古くなるので、判断はここで行う。
//   - トークンが無効・期限切れ → 401 (クライアントが、再認可できるように WWW-Authenticate を付ける)
//   - scope があるのに mcp が入っていない → 403 (読み取りだけに同意したトークンでは、管理の操作をさせない)
//   - ロールが ADMIN_ROLE_IDS のどれでもない → 403
//   - auth に尋ねられない、またはロールを確かめられていない → 503 (権限を、安全な側に倒す)

const introspectTimeout = 3 * time.Second

// Principal は、認証できた利用者。
type Principal struct {
	ID   string
	Name string
}

func (p *Principal) String() string { return fmt.Sprintf("%s (id %s)", cleanText(p.Name, 40), p.ID) }

type principalKey struct{}

func principalOf(r *http.Request) *Principal {
	p, _ := r.Context().Value(principalKey{}).(*Principal)
	return p
}

type Authz struct {
	url         string
	admins      map[string]bool
	requireLive bool
	http        *http.Client
}

func NewAuthz(cfg *Config) *Authz {
	admins := map[string]bool{}
	for _, r := range cfg.ConnectorConfig.AdminRoles {
		admins[r] = true
	}
	return &Authz{url: cfg.ConnectorConfig.AuthInternalURL, admins: admins, requireLive: cfg.ConnectorConfig.RequireLiveRoles, http: &http.Client{Timeout: introspectTimeout}}
}

type authResult int

const (
	authOK authResult = iota
	authInvalid
	authForbidden
	authUnavailable
)

// Check は Bearer トークンを確かめる。
func (a *Authz) Check(ctx context.Context, token string) (*Principal, authResult, string) {
	if token == "" {
		return nil, authInvalid, "Bearer トークンが要ります"
	}
	body, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+"/introspect", bytes.NewReader(body))
	if err != nil {
		return nil, authUnavailable, "auth への依頼を作れません"
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.http.Do(req)
	if err != nil {
		return nil, authUnavailable, "auth に尋ねられません"
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, authUnavailable, fmt.Sprintf("auth が %d を返しました", res.StatusCode)
	}
	var out struct {
		Active bool `json:"active"`
		Claims struct {
			ID          string   `json:"id"`
			Username    string   `json:"username"`
			Roles       []string `json:"roles"`
			RolesSource string   `json:"roles_source"`
			Scope       *string  `json:"scope"`
		} `json:"claims"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(&out); err != nil {
		return nil, authUnavailable, "auth の返事を読めません"
	}
	if !out.Active || out.Claims.ID == "" {
		return nil, authInvalid, "トークンが無効か、期限切れです"
	}
	if !scopeAllows(out.Claims.Scope) {
		return nil, authForbidden, "トークンの scope に mcp がありません"
	}
	if a.requireLive && out.Claims.RolesSource != "live" {
		// auth が Discord でロールを確かめていない (古いトークンの値や、障害)。Admin かを判断できない
		return nil, authUnavailable, "ロールを確かめられません (auth が Discord に尋ねられない)"
	}
	for _, r := range out.Claims.Roles {
		if a.admins[r] {
			return &Principal{ID: out.Claims.ID, Name: out.Claims.Username}, authOK, ""
		}
	}
	return nil, authForbidden, "Admin 以上のロールが要ります"
}

// scopeAllows は、トークンの scope がコネクタの操作を許しているか。
// scope の無いトークンは、本人が auth で発行した 90 日トークン。ある場合は、OAuth の同意の範囲 (空白区切り) に mcp が要る。
func scopeAllows(scope *string) bool {
	if scope == nil {
		return true
	}
	return slices.Contains(strings.Fields(*scope), "mcp")
}

// Middleware は、すべての経路に認証を掛け、認証できた人を、リクエストに付ける。本文の大きさにも上限を掛ける。
func (a *Authz) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			token = ""
		}
		p, res, why := a.Check(r.Context(), token)
		switch res {
		case authInvalid:
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeJSONResp(w, http.StatusUnauthorized, map[string]any{"error": why})
			return
		case authForbidden:
			writeJSONResp(w, http.StatusForbidden, map[string]any{"error": why})
			return
		case authUnavailable:
			w.Header().Set("Retry-After", "5")
			writeJSONResp(w, http.StatusServiceUnavailable, map[string]any{"error": why})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}
