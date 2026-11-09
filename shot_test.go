package main

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCoreBoxIgnoresFarOutliers(t *testing.T) {
	s := &Stats{X0: -5712, X1: -5602, Z0: -10210, Z1: -10048, Y0: 55, Y1: 94}
	for x := -5650; x < -5630; x++ {
		for z := -10140; z < -10120; z++ {
			s.Columns = append(s.Columns, Column{X: x, Z: z, N: 10, Y0: 60, Y1: 70})
		}
	}
	// 遠くに 1 列だけ (全体の 0.03% 以下)
	s.Columns = append(s.Columns, Column{X: -5712, Z: -10210, N: 1, Y0: 60, Y1: 60})
	b := CoreBox(s)
	if b[0] < -5655 || b[2] > -5627 || b[1] < -10145 || b[3] > -10117 {
		t.Fatalf("box は中心の塊に収まるはず: %v", b)
	}
	if b[0] >= b[2] || b[1] >= b[3] {
		t.Fatalf("box が逆: %v", b)
	}
}

func TestCoreBoxWithoutColumnsUsesRange(t *testing.T) {
	s := &Stats{X0: 1, X1: 9, Z0: 2, Z1: 8}
	if b := CoreBox(s); b != [4]int{1, 2, 10, 9} {
		t.Fatalf("box: %v", b)
	}
}

func TestAnnotForLimitsPillars(t *testing.T) {
	s := &Stats{X0: 0, X1: 100, Z0: 0, Z1: 100, Y0: 60, Y1: 70}
	for i := 0; i < maxPillars+500; i++ {
		s.Columns = append(s.Columns, Column{X: i % 100, Z: i / 100, N: 1 + i%7, Y0: 60, Y1: 62})
	}
	a := AnnotFor(s, "x", "y", true)
	if len(a.Pillars) != maxPillars {
		t.Fatalf("pillars=%d", len(a.Pillars))
	}
	if b := AnnotFor(s, "x", "y", false); len(b.Pillars) != 0 {
		t.Fatalf("復旧後の画面に赤い線は要らない: %d", len(b.Pillars))
	}
	arg := a.overlayArg()
	if arg["label"] != "x" || len(arg["pillars"].([]map[string]int)) != maxPillars {
		t.Fatalf("arg: %v", arg["label"])
	}
}

func TestPageURLTurnsSignsOff(t *testing.T) {
	s := &Shooter{URL: "http://3d-map.torosaba.net/"}
	got := s.pageURL("overworld", Cam{X: 1, Y: 2, Z: 3, Dist: 4})
	if !strings.HasPrefix(got, "http://3d-map.torosaba.net/?marumasign3d=off#overworld:1:2:3:4:") {
		t.Fatal(got)
	}
	if !strings.Contains(prepareShotJS, "window.marumasign3d.disable()") {
		t.Fatal("撮る前に看板を消す")
	}
}

func TestRemoteBrowserURL(t *testing.T) {
	const path = "/devtools/browser/0a1b-2c3d"
	var host string
	reply := `{"webSocketDebuggerUrl":"ws://evil.example:1234` + path + `"}`
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		if r.URL.Path != "/json/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if code == http.StatusFound {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	ctx := context.Background()

	// 答えのホストは使わず、設定のホストに、パスだけをつなぐ
	got, err := remoteBrowserURL(ctx, "ws://"+addr)
	if err != nil || got != "ws://"+addr+path {
		t.Fatalf("%q %v", got, err)
	}
	if host != "localhost" {
		t.Fatalf("Host %q (Chrome は IP か localhost しか受けない)", host)
	}
	// パスまで書いてあれば、尋ねない
	direct := "ws://" + addr + path
	if got, err := remoteBrowserURL(ctx, direct); err != nil || got != direct {
		t.Fatalf("%q %v", got, err)
	}
	for name, bad := range map[string]string{
		"JSON でない": "Host header is specified and is not an IP address",
		"項目が無い":    `{"Browser":"x"}`,
		"型が違う":     `{"webSocketDebuggerUrl":42}`,
		"パスが違う":    `{"webSocketDebuggerUrl":"ws://x/other"}`,
		"パスが長い":    `{"webSocketDebuggerUrl":"ws://x/devtools/browser/a/../../b"}`,
	} {
		reply = bad
		if got, err := remoteBrowserURL(ctx, "ws://"+addr); err == nil {
			t.Errorf("%s: %q", name, got)
		}
	}
	// 転送は追わない
	reply, code = `{"webSocketDebuggerUrl":"ws://x`+path+`"}`, http.StatusFound
	if got, err := remoteBrowserURL(ctx, "ws://"+addr); err == nil {
		t.Errorf("転送: %q", got)
	}
	if _, err := remoteBrowserURL(ctx, "ws://"); err == nil {
		t.Fatal("ホストの無い URL")
	}
}

// 実物のブラウザで試す。CI では飛ばす。
//
//	BROWSER_TEST_URL=ws://127.0.0.1:9222   起動してあるブラウザにつなぐ (本番と同じ形)
//	CHROME_TEST_PATH=/path/to/chrome       ローカルの Chromium を起動する (開発用)
//
// 画面の大きさ、看板を消す呼び出し、撮影のあとにタブが残らないことを確かめる。
func TestShotOnRealBrowser(t *testing.T) {
	browser, chrome := os.Getenv("BROWSER_TEST_URL"), os.Getenv("CHROME_TEST_PATH")
	if browser == "" && chrome == "" {
		t.Skip("BROWSER_TEST_URL も CHROME_TEST_PATH も無い")
	}
	const page = `<!doctype html><html><body style="margin:0"><div id="map-container" style="width:100%;height:100vh;background:#123"></div><div id="app">UI</div>
<script>
window.bluemap = {mapViewer:{map:{hiresTileManager:{tiles:new Map([[0,{loading:false}]])}}}};
window.marumasign3d = {disable(){ fetch('/disabled' + location.search + '&w=' + innerWidth + '&h=' + innerHeight); }};
</script></body></html>`
	var mu sync.Mutex
	var disabled []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/disabled" {
			mu.Lock()
			disabled = append(disabled, r.URL.RawQuery)
			mu.Unlock()
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	s := &Shooter{URL: srv.URL, Browser: browser, Chrome: chrome, W: 640, H: 360, MaxWait: 5 * time.Second}
	// ローカルの Chromium は、撮るたびに起動する。マシンが忙しいと 1 回に 1 分近くかかる
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	out := filepath.Join(t.TempDir(), "shot.png")
	for i := 0; i < 2; i++ { // 2 回続けて撮れる (前のコンテキストが残らない)
		if err := s.Shot(ctx, "overworld", Cam{X: 1, Y: 70, Z: 2, Dist: 100}, out, nil); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds().Size(); got.X != 640 || got.Y != 360 {
		t.Fatalf("画像の大きさ %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(disabled) != 2 || disabled[0] != "marumasign3d=off&w=640&h=360" {
		t.Fatalf("disable の呼び出し: %q", disabled)
	}
	if browser == "" {
		return // ローカルの Chromium は、撮るたびに終わる
	}
	// 閉じたあと、タブが残っていない
	ws, err := remoteBrowserURL(ctx, browser)
	if err != nil {
		t.Fatal(err)
	}
	host := strings.SplitN(strings.TrimPrefix(ws, "ws://"), "/", 2)[0]
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/json/list", nil)
	req.Host = "localhost"
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var list bytes.Buffer
	_, _ = list.ReadFrom(res.Body)
	if strings.Contains(list.String(), srv.URL) {
		t.Fatalf("タブが残っている: %s", list.String())
	}
}
