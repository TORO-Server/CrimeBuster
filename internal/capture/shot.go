package capture

import (
	"context"
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"encoding/json"
	"fmt"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Cam は BlueMap のカメラ。URL の # のあとの値と同じ意味 (BlueMapApp.ts の updatePageAddress)。
type Cam struct {
	X, Y, Z float64
	Dist    float64
	Rot     float64 // 向き (ラジアン)
	Angle   float64 // 見下ろす角 (0 が真上、π/2 が水平)
}

func (c Cam) Hash(mapID string) string {
	return fmt.Sprintf("#%s:%.0f:%.0f:%.0f:%.0f:%.2f:%.2f:0:0:perspective", mapID, c.X, c.Y, c.Z, c.Dist, c.Rot, c.Angle)
}

// CamFor は荒らされた場所の中心が画面に収まるカメラ。
// 壊した場所 (x, z ごとの数) の重み付きの中央値を中心にし、遠くに離れた少数の点は無視する (範囲の外れ値で遠くなりすぎないように)。
// 列が無いときは、範囲 (x0..x1, z0..z1) の真ん中を使う。
func CamFor(s *detect.Stats) Cam {
	cx, cz := float64(s.X0+s.X1)/2, float64(s.Z0+s.Z1)/2
	span := math.Max(float64(s.X1-s.X0), float64(s.Z1-s.Z0))
	if len(s.Columns) > 0 {
		cx, cz = weightedMedian(s.Columns, func(c detect.Column) int { return c.X }), weightedMedian(s.Columns, func(c detect.Column) int { return c.Z })
		span = 2 * weightedRadius(s.Columns, cx, cz, 0.8) // 赤い印があるので、広めに撮っても、荒らされた所は見える
	}
	dist := math.Min(math.Max(span*1.1+40, 70), 600)
	y := math.Min(math.Max(float64(s.Y0+s.Y1)/2, 62), 90)
	return Cam{X: cx, Y: y, Z: cz, Dist: dist, Rot: -0.6, Angle: 0.75}
}

// CloseCamFor は、いちばん壊された所 (12 ブロック四方で壊した数が最大の区画) に寄ったカメラ。
// 荒らされた範囲が広いと、全体の画面では 1 か所ずつが小さいので、寄った画面も足す。
// 範囲が狭くて全体の画面と変わらないとき (全体の距離が近いとき) は、false。
func CloseCamFor(s *detect.Stats, overview Cam) (Cam, bool) {
	if len(s.Columns) == 0 || overview.Dist < 130 {
		return Cam{}, false
	}
	const cell = 12
	type key struct{ a, b int }
	type acc struct{ n, sx, sz, sy int }
	cells := map[key]*acc{}
	for _, c := range s.Columns {
		k := key{floorDiv(c.X, cell), floorDiv(c.Z, cell)}
		a := cells[k]
		if a == nil {
			a = &acc{}
			cells[k] = a
		}
		a.n += c.N
		a.sx += c.X * c.N
		a.sz += c.Z * c.N
		a.sy += c.Y1 * c.N
	}
	var best *acc
	for _, a := range cells {
		if best == nil || a.n > best.n {
			best = a
		}
	}
	y := math.Min(math.Max(float64(best.sy)/float64(best.n), 0), 300)
	return Cam{X: float64(best.sx) / float64(best.n), Y: y, Z: float64(best.sz) / float64(best.n), Dist: 45, Rot: -0.6, Angle: 0.9}, true
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// CoreBox は、壊した数の 97% が入る範囲の枠 (x0, z0, x1, z1)。遠くに離れた少数の点で、枠が画面を埋めないように、
// 外側の 1.5% ずつを除く。列が無いときは、範囲そのもの。
func CoreBox(s *detect.Stats) [4]int {
	if len(s.Columns) == 0 {
		return [4]int{s.X0, s.Z0, s.X1 + 1, s.Z1 + 1}
	}
	kx := func(c detect.Column) int { return c.X }
	kz := func(c detect.Column) int { return c.Z }
	return [4]int{
		int(weightedQuantile(s.Columns, kx, 0.015)) - 1, int(weightedQuantile(s.Columns, kz, 0.015)) - 1,
		int(weightedQuantile(s.Columns, kx, 0.985)) + 2, int(weightedQuantile(s.Columns, kz, 0.985)) + 2,
	}
}

// weightedQuantile は、壊した数で重みを付けた q (0〜1) の分位点。
func weightedQuantile(cols []detect.Column, key func(detect.Column) int, q float64) float64 {
	idx := make([]int, len(cols))
	total := 0
	for i, c := range cols {
		idx[i] = i
		total += c.N
	}
	sort.Slice(idx, func(a, b int) bool { return key(cols[idx[a]]) < key(cols[idx[b]]) })
	acc := 0
	for _, i := range idx {
		acc += cols[i].N
		if float64(acc) >= q*float64(total) {
			return float64(key(cols[i]))
		}
	}
	return float64(key(cols[idx[len(idx)-1]]))
}

func weightedMedian(cols []detect.Column, key func(detect.Column) int) float64 {
	idx := make([]int, len(cols))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return key(cols[idx[a]]) < key(cols[idx[b]]) })
	total := 0
	for _, c := range cols {
		total += c.N
	}
	acc := 0
	for _, i := range idx {
		acc += cols[i].N
		if acc*2 >= total {
			return float64(key(cols[i]))
		}
	}
	return 0
}

// weightedRadius は、中心から、壊した数の p (0〜1) の割合が入る水平の距離。
func weightedRadius(cols []detect.Column, cx, cz, p float64) float64 {
	type dw struct {
		d float64
		n int
	}
	ds := make([]dw, len(cols))
	total := 0
	for i, c := range cols {
		ds[i] = dw{math.Hypot(float64(c.X)-cx, float64(c.Z)-cz), c.N}
		total += c.N
	}
	sort.Slice(ds, func(a, b int) bool { return ds[a].d < ds[b].d })
	acc := 0
	for _, x := range ds {
		acc += x.n
		if float64(acc) >= p*float64(total) {
			return x.d
		}
	}
	return 0
}

// Shooter は BlueMap の web を開いて、画面を撮る (Chromium)。
// 本番のブラウザは別のコンテナ (BROWSER_URL) で動かす。プレイヤーの置いた看板を読むページを開くので、crime-buster と同じ権限に置かない。
type Shooter struct {
	URL     string
	Browser string // BROWSER_URL。空なら、ローカルの Chromium を起動する (開発用)
	Resolve string // ローカルの Chromium だけ。"toro-edge:80" のように、URL の名前をこの先へ向ける
	Chrome  string
	Flags   []string
	W, H    int
	MaxWait time.Duration

	LastOverlay string // 直前に印を描いた結果 (確認用)
}

func NewShooter(c *config.Config) *Shooter {
	var flags []string
	for _, f := range strings.Split(os.Getenv("CHROME_FLAGS"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			flags = append(flags, f)
		}
	}
	return &Shooter{URL: c.ShotConfig.BlueMapURL, Browser: c.ShotConfig.BrowserURL, Resolve: c.ShotConfig.BlueMapResolve, Chrome: c.ShotConfig.ChromePath, Flags: flags, W: c.ShotConfig.ShotWidth, H: c.ShotConfig.ShotHeight, MaxWait: c.ShotConfig.ShotSettle}
}

// tilesLoadedJS は、いま見えている範囲のタイルが全部読み終わったか。
const tilesLoadedJS = `(() => {
  const b = window.bluemap;
  if (!b || !b.mapViewer || !b.mapViewer.map) return false;
  const m = b.mapViewer.map;
  const mgrs = [m.hiresTileManager].concat(m.lowresTileManager || []);
  let n = 0, loading = 0;
  for (const g of mgrs) {
    if (!g || !g.tiles) continue;
    for (const t of g.tiles.values()) { n++; if (t.loading) loading++; }
  }
  return n > 0 && loading === 0;
})()`

// prepareShotJS は、撮る直前に看板を消し (プレイヤーの選んだ画像を証拠に写さない)、操作の部品 (#app 以下) を隠して、地図だけにする。
const prepareShotJS = `(() => {
  if (window.marumasign3d) window.marumasign3d.disable();
  const s = document.createElement('style');
  s.textContent = '#app{display:none!important}';
  document.head.appendChild(s);
})()`

// Pillar は、壊された 1 列 (x, z) とその高さ。画面では、縦の赤い線になる。
type Pillar struct {
	X, Z   int
	Y0, Y1 int
	N      int
}

// Annot は、撮った画面の上に重ねる印。BlueMap は「いまの地形」しか描かないので、穴や欠けだけでは
// どこが荒らされたか分からない。壊された列を赤い線で、荒らされた範囲を黄色の枠で示す。
// 文字は ASCII だけ (実行する環境には日本語のフォントが無い)。
type Annot struct {
	Label    string   // 左上に書く 1 行目
	Sub      string   // 2 行目
	Pillars  []Pillar // 赤い線 (復旧後の画面では空)
	Box      [4]int   // 枠: x0, z0, x1, z1
	BoxY     int      // 枠を置く高さ
	BoxColor string   // 枠の色 (CSS)
	NoBox    bool     // 枠を描かない (寄った画面では、枠が画面を横切るだけなので)
}

// maxPillars は、1 枚に描く列の数の上限 (多いときは、壊した数が多い列を残す)。
const maxPillars = 6000

// AnnotFor は Stats から、被害の画面に重ねる印を作る。
func AnnotFor(s *detect.Stats, label, sub string, withPillars bool) *Annot {
	a := &Annot{Label: label, Sub: sub, Box: CoreBox(s), BoxY: s.Y0, BoxColor: "#ffd400"}
	if withPillars {
		for _, c := range s.TopBreakColumns(maxPillars) {
			a.Pillars = append(a.Pillars, Pillar{X: c.X, Z: c.Z, Y0: c.Y0, Y1: c.Y1, N: c.N})
		}
	}
	return a
}

// overlayJS は、いまのカメラの行列で、世界の座標を画面の点にして、全画面の canvas に描く。
// 返す文字列は、描いた結果 (ok:… または、できなかった理由)。
const overlayJS = `((a) => {
  const b = window.bluemap;
  const cam = b && b.mapViewer && b.mapViewer.camera;
  if (!cam) return 'no camera';
  cam.updateMatrixWorld(true);
  const V = cam.matrixWorldInverse.elements, P = cam.projectionMatrix.elements;
  const W = window.innerWidth, H = window.innerHeight;
  const proj = (x, y, z) => {
    const vx = V[0]*x + V[4]*y + V[8]*z + V[12], vy = V[1]*x + V[5]*y + V[9]*z + V[13],
          vz = V[2]*x + V[6]*y + V[10]*z + V[14], vw = V[3]*x + V[7]*y + V[11]*z + V[15];
    const cx = P[0]*vx + P[4]*vy + P[8]*vz + P[12]*vw, cy = P[1]*vx + P[5]*vy + P[9]*vz + P[13]*vw,
          cw = P[3]*vx + P[7]*vy + P[11]*vz + P[15]*vw;
    if (!(cw > 0)) return null;
    return [(cx / cw * 0.5 + 0.5) * W, (1 - (cy / cw * 0.5 + 0.5)) * H];
  };
  const cv = document.createElement('canvas');
  cv.width = W; cv.height = H;
  cv.style.cssText = 'position:fixed;left:0;top:0;width:' + W + 'px;height:' + H + 'px;z-index:2147483647;pointer-events:none';
  document.body.appendChild(cv);
  const g = cv.getContext('2d');
  let drawn = 0;
  g.lineCap = 'round';
  for (const p of (a.pillars || [])) {
    const lo = proj(p.x + 0.5, p.y0, p.z + 0.5), hi = proj(p.x + 0.5, p.y1 + 1, p.z + 0.5);
    if (!lo || !hi) continue;
    g.strokeStyle = 'rgba(255,30,30,0.55)'; g.lineWidth = 4;
    g.beginPath(); g.moveTo(lo[0], lo[1]); g.lineTo(hi[0], hi[1]); g.stroke();
    g.fillStyle = 'rgba(255,60,60,0.75)';
    g.beginPath(); g.arc(hi[0], hi[1], 3, 0, 6.2832); g.fill();
    drawn++;
  }
  const [x0, z0, x1, z1] = a.box || [0, 0, 0, 0], by = a.boxY;
  const cs = a.box ? [proj(x0, by, z0), proj(x1, by, z0), proj(x1, by, z1), proj(x0, by, z1)] : [];
  if (a.box && cs.every(Boolean)) {
    g.beginPath(); g.moveTo(cs[0][0], cs[0][1]); for (let i = 1; i < 4; i++) g.lineTo(cs[i][0], cs[i][1]); g.closePath();
    g.setLineDash([16, 10]); g.lineWidth = 7; g.strokeStyle = 'rgba(0,0,0,0.7)'; g.stroke();
    g.lineWidth = 4; g.strokeStyle = a.boxColor; g.stroke(); g.setLineDash([]);
  }
  const lines = [a.label, a.sub].filter(Boolean);
  g.font = 'bold 22px monospace';
  const tw = Math.max(...lines.map(s => g.measureText(s).width)) + 24;
  g.fillStyle = 'rgba(10,10,14,0.82)'; g.fillRect(12, 12, tw, 16 + lines.length * 30);
  lines.forEach((s, i) => { g.fillStyle = i === 0 ? '#ff5a5a' : '#e8e8ee'; g.fillText(s, 24, 42 + i * 30); });
  return 'ok:' + drawn + ' pillars, box=' + (a.box ? cs.every(Boolean) : 'none');
})`

// overlayArg は、JS に渡す形 (キーは小文字)。
func (a *Annot) overlayArg() map[string]any {
	ps := make([]map[string]int, 0, len(a.Pillars))
	for _, p := range a.Pillars {
		ps = append(ps, map[string]int{"x": p.X, "z": p.Z, "y0": p.Y0, "y1": p.Y1, "n": p.N})
	}
	arg := map[string]any{"label": a.Label, "sub": a.Sub, "pillars": ps, "boxY": a.BoxY, "boxColor": a.BoxColor}
	if !a.NoBox {
		arg["box"] = a.Box
	}
	return arg
}

// browserPathRE は、DevTools のブラウザの接続先のパス。
var browserPathRE = regexp.MustCompile(`^/devtools/browser/[0-9A-Za-z-]{1,64}$`)

// remoteBrowserURL は、ブラウザのコンテナの /json/version から、接続先のパスだけを読む。
// あちらはプレイヤーの看板を読む側なので信用しない: 答えのホストは使わず、転送も追わず、形がおかしければエラーにする。
func remoteBrowserURL(ctx context.Context, browser string) (string, error) {
	u, err := url.Parse(browser)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("BROWSER_URL の形が正しくありません")
	}
	if browserPathRE.MatchString(u.Path) {
		return browser, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+u.Host+"/json/version", nil)
	if err != nil {
		return "", err
	}
	req.Host = "localhost" // Chrome は、IP か localhost の Host しか受けない
	hc := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("browser: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("browser: /json/version: HTTP %d", res.StatusCode)
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&v); err != nil {
		return "", fmt.Errorf("browser: /json/version を読めません: %w", err)
	}
	w, err := url.Parse(v.WS)
	if err != nil || !browserPathRE.MatchString(w.Path) {
		return "", fmt.Errorf("browser: /json/version の接続先の形が正しくありません")
	}
	return (&url.URL{Scheme: "ws", Host: u.Host, Path: w.Path}).String(), nil
}

// openRemote は、別のコンテナのブラウザに、1 枚分の新しいブラウザコンテキストを作る。閉じると、Cookie もキャッシュも残らない。
func (s *Shooter) openRemote(ctx context.Context) (context.Context, func(), error) {
	ws, err := remoteBrowserURL(ctx, s.Browser)
	if err != nil {
		return nil, nil, err
	}
	actx, cancelA := remote.NewAllocator(ctx, ws)
	cctx, cancelC := chromedp.NewContext(actx, chromedp.WithNewBrowserContext())
	closeAll := func() { cancelC(); cancelA() }
	err = chromedp.Do(cctx)
	if err == nil {
		return cctx, closeAll, nil
	}
	if !strings.Contains(err.Error(), "no browser is open") {
		closeAll()
		return nil, nil, fmt.Errorf("browser: %w", err)
	}
	// フル版の Chrome は、ウィンドウの無いコンテキストに、新しいウィンドウでしかタブを作れない (headless-shell は newWindow を受けない)
	c := chromedp.FromContext(cctx)
	newWindow := true
	created, err := cdp.Call(cctx, c.Browser, target.CreateTarget, target.CreateTargetParams{
		URL:              "about:blank",
		BrowserContextID: c.BrowserContextID,
		NewWindow:        &newWindow,
	})
	if err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("browser: %w", err)
	}
	tctx, cancelT := chromedp.NewContext(cctx, chromedp.WithTargetID(created.TargetID))
	return tctx, func() { cancelT(); closeAll() }, nil
}

// openBrowser は、撮影 1 回分のブラウザの context を作る。返す関数で閉じる。
func (s *Shooter) openBrowser(ctx context.Context) (context.Context, func(), error) {
	if s.Browser != "" {
		return s.openRemote(ctx)
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.WindowSize(s.W, s.H))
	if s.Chrome != "" {
		opts = append(opts, chromedp.ExecPath(s.Chrome))
	}
	if s.Resolve != "" {
		if u, err := url.Parse(s.URL); err == nil {
			opts = append(opts, chromedp.Flag("host-resolver-rules", fmt.Sprintf("MAP %s %s", u.Hostname(), s.Resolve)))
		}
	}
	for _, f := range s.Flags {
		k, v, hasV := strings.Cut(f, "=")
		if hasV {
			opts = append(opts, chromedp.Flag(k, v))
		} else {
			opts = append(opts, chromedp.Flag(k, true))
		}
	}
	actx, cancelA := chromedp.NewExecAllocator(ctx, opts...)
	cctx, cancelC := chromedp.NewContext(actx)
	return cctx, func() { cancelC(); cancelA() }, nil
}

// pageURL は撮る地図の URL。?marumasign3d=off で、看板の処理を始めさせない (プレイヤーの指定した画像や動画を、ブラウザに読ませない)。
func (s *Shooter) pageURL(mapID string, cam Cam) string {
	return strings.TrimRight(s.URL, "/") + "/?marumasign3d=off" + cam.Hash(mapID)
}

// Shot は地図を開いて、タイルが読み終わるまで待ち、(ann があれば印を重ねて) PNG を out に書く。
func (s *Shooter) Shot(ctx context.Context, mapID string, cam Cam, out string, ann *Annot) error {
	cctx, closeBrowser, err := s.openBrowser(ctx)
	if err != nil {
		return err
	}
	defer closeBrowser()
	cctx, cancelT := context.WithTimeout(cctx, s.MaxWait+90*time.Second)
	defer cancelT()

	target := s.pageURL(mapID, cam)
	// 画面の大きさは、起動のフラグではなく emulation で決める (別のコンテナのブラウザのフラグは変えられない)
	if err := chromedp.Do(cctx,
		chromedp.EmulateViewport(int64(s.W), int64(s.H)),
		chromedp.Navigate(target),
		chromedp.WaitVisible(chromedp.CSS("#map-container")),
	); err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}

	// 読み込み中の見え方が安定するまで (タイルが全部読み終わるまで) 待つ
	deadline := time.Now().Add(s.MaxWait)
	stable := 0
	for time.Now().Before(deadline) {
		ok, err := chromedp.Run(cctx, chromedp.Evaluate[bool](tilesLoadedJS))
		if err != nil {
			return err
		}
		if ok {
			stable++
			if stable >= 3 {
				break
			}
		} else {
			stable = 0
		}
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-cctx.Done():
			return cctx.Err()
		}
	}

	if ann != nil {
		arg, err := json.Marshal(ann.overlayArg())
		if err != nil {
			return err
		}
		// 印が描けなくても、印の無い画面は撮る (無いよりよい)。結果は LastOverlay に残す
		res, err := chromedp.Run(cctx, chromedp.Evaluate[string](overlayJS+"("+string(arg)+")"))
		if err != nil {
			res = "failed: " + err.Error()
		}
		s.LastOverlay = res
	}

	if err := chromedp.Do(cctx,
		chromedp.Evaluate[chromedp.Void](prepareShotJS),
		chromedp.Sleep(1500*time.Millisecond),
	); err != nil {
		return err
	}
	buf, err := chromedp.Run(cctx, chromedp.FullScreenshot(100)) // 100 は PNG
	if err != nil {
		return err
	}
	return os.WriteFile(out, buf, 0o644)
}
