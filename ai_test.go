package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude は Messages API の代わりに、決めた文章を返し、受けた要求を覚える。
func fakeClaude(t *testing.T, answer string, got *map[string]any, hdr *http.Header) *AI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if hdr != nil {
			*hdr = r.Header.Clone()
		}
		if got != nil {
			_ = json.NewDecoder(r.Body).Decode(got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]any{{"type": "text", "text": answer}}})
	}))
	t.Cleanup(srv.Close)
	return NewAI(&Config{AIConfig: AIConfig{AIKey: "k-test", AIModel: "claude-haiku-5-5", AIBaseURL: srv.URL}})
}

func TestNewAIWithoutKeyIsNil(t *testing.T) {
	if NewAI(&Config{}) != nil {
		t.Fatal("キーが無ければ AI は使わない")
	}
}

func TestReviewIncident(t *testing.T) {
	var req map[string]any
	var hdr http.Header
	ai := fakeClaude(t, "判定です:\n{\"verdict\":\"grief\",\"confidence\":92,\"summary\":\"他人の建物を @everyone 広く壊している\"}\n以上", &req, &hdr)
	img := filepath.Join(t.TempDir(), "a.png")
	_ = os.WriteFile(img, []byte("PNG"), 0o644)
	s := &Stats{User: UserInfo{Name: "x"}, World: "main", From: 1762221000, To: 1762221360, Breaks: 2641, PeakPerS: 421, TopBreaks: []MatCount{{Material: "stone", Count: 820}}}
	v, err := ai.ReviewIncident(context.Background(), s, "burst-break", []string{img, "", filepath.Join(t.TempDir(), "none.png")})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != "grief" || v.Confidence != 92 || strings.Contains(v.Summary, "@everyone") {
		t.Fatalf("v=%+v (メンションは無効にする)", v)
	}
	if hdr.Get("x-api-key") != "k-test" || hdr.Get("anthropic-version") == "" {
		t.Fatalf("headers: %v", hdr)
	}
	if req["model"] != "claude-haiku-5-5" {
		t.Fatalf("model: %v", req["model"])
	}
	parts := req["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["type"] != "image" || parts[1].(map[string]any)["type"] != "text" {
		t.Fatalf("画像 1 枚 + 文章 1 つのはず (読めない画像は飛ばす): %v", parts)
	}
	if !v.Allows(70) || v.Allows(95) {
		t.Fatal("Allows の基準")
	}
}

func TestVerdictAllows(t *testing.T) {
	var nilV *Verdict
	if nilV.Allows(0) {
		t.Fatal("nil は許さない")
	}
	if (&Verdict{Verdict: "legit", Confidence: 99}).Allows(70) || (&Verdict{Verdict: "unclear", Confidence: 99}).Allows(70) {
		t.Fatal("荒らし以外は許さない")
	}
}

func TestReviewIncidentRejectsUnknownVerdictAndBadAnswer(t *testing.T) {
	if _, err := fakeClaude(t, `{"verdict":"maybe","confidence":50,"summary":""}`, nil, nil).ReviewIncident(context.Background(), &Stats{}, "x", nil); err == nil {
		t.Fatal("知らない判定は、エラー")
	}
	if _, err := fakeClaude(t, "わかりません", nil, nil).ReviewIncident(context.Background(), &Stats{}, "x", nil); err == nil {
		t.Fatal("JSON が無い答えは、エラー")
	}
}

func TestAIAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()
	ai := NewAI(&Config{AIConfig: AIConfig{AIKey: "bad", AIModel: "m", AIBaseURL: srv.URL}})
	_, err := ai.ReviewIncident(context.Background(), &Stats{}, "x", nil)
	if err == nil || !strings.Contains(err.Error(), "authentication_error") {
		t.Fatalf("err=%v", err)
	}
}

func TestReviewChat(t *testing.T) {
	var req map[string]any
	ai := fakeClaude(t, `{"flags":[{"id":1,"category":"Harassment","confidence":88,"summary":"特定の人への中傷"},{"id":9,"category":"spam","confidence":99,"summary":"範囲外"},{"id":-1,"category":"x","confidence":1,"summary":"y"}]}`, &req, nil)
	msgs := []ChatMsg{{Time: 1762221000, User: 1, Msg: "こんにちは"}, {Time: 1762221005, User: 2, Msg: "ひどい発言\nの例"}}
	flags, err := ai.ReviewChat(context.Background(), msgs, map[int]string{1: "alice", 2: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) != 1 || flags[0].ID != 1 || flags[0].Category != "harassment" || flags[0].Confidence != 88 {
		t.Fatalf("範囲外の番号は捨てる: %+v", flags)
	}
	// category は報告文に載るので、決めた言葉だけにする
	for in, want := range map[string]string{"Spam": "spam", " hate ": "hate", "other": "other", "[x](https://example.com)": "other", "": "other"} {
		if got := chatCategory(in); got != want {
			t.Errorf("chatCategory(%q) = %q", in, got)
		}
	}
	text := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "[1] ") || !strings.Contains(text, "bob: ひどい発言 の例") || strings.Count(text, "\n") != 2 {
		t.Fatalf("発言は 1 行ずつ、番号つきで渡す: %q", text)
	}
}

func TestReportShowsAIAndSkip(t *testing.T) {
	s := &Stats{User: UserInfo{Name: "x", UUID: "u"}, World: "main", From: 1762221000, To: 1762221360, Breaks: 100}
	txt := BuildReport(ReportInput{Stats: s, Trigger: "burst", Mode: ModeLive, Out: Outcome{
		Cmd: "co rollback u:x t:10s r:#main", AI: &Verdict{Verdict: "legit", Confidence: 80, Summary: "自分の建築の整地"},
		Skipped: "AI の判定が「正当な作業」",
	}})
	if !strings.Contains(txt, "AI の判定**: 正当な作業 (確からしさ 80%)") || !strings.Contains(txt, "自動ロールバックはしていません") {
		t.Fatalf("report: %s", txt)
	}
	if strings.Count(txt, "**座標**") > 1 {
		t.Fatalf("座標が重複: %s", txt)
	}
}

func TestCloseCamFor(t *testing.T) {
	s := &Stats{}
	for x := 0; x < 200; x++ { // 薄く広い
		s.Columns = append(s.Columns, Column{X: x, Z: 0, N: 1, Y0: 60, Y1: 64})
	}
	for x := -30; x < -20; x++ { // 濃い所
		for z := 50; z < 60; z++ {
			s.Columns = append(s.Columns, Column{X: x, Z: z, N: 20, Y0: 60, Y1: 70})
		}
	}
	if _, ok := CloseCamFor(s, Cam{Dist: 100}); ok {
		t.Fatal("全体が近いときは、寄った画面は要らない")
	}
	c, ok := CloseCamFor(s, Cam{Dist: 300})
	if !ok || c.X > -20 || c.X < -30 || c.Z < 50 || c.Z > 60 || c.Y != 70 {
		t.Fatalf("いちばん濃い所に寄る: %+v ok=%v", c, ok)
	}
	if floorDiv(-1, 12) != -1 || floorDiv(-12, 12) != -1 || floorDiv(-13, 12) != -2 || floorDiv(11, 12) != 0 {
		t.Fatal("floorDiv")
	}
}
