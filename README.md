# CrimeBuster — 荒らしを見つけて、証拠の画像を付けて通報し、ロールバックする

TORO サーバーの CoreProtect の記録を監視し、人の手では出せない速さでブロックを壊したり置いたりしている人を見つけたら、

1. 荒らされた場所の画像 (BlueMap の 3D の地図) を撮り、
2. その人のブロックの変更を CoreProtect でロールバックし、
3. 復旧後の画像を撮り、
4. Discord の荒らし通報フォーラムに、投稿を立てて報告する (「ロルバ済」のタグ付き)。

言語は Go。`crime-buster.exe` / `crime-buster` が 1 つ。イメージの中身は、実行ファイルと CA 証明書だけ (`FROM scratch`、非 root)。コンソールの中継 (`toro-console`) は同じイメージの別のコンテナで動かし、BlueMap の画面を撮るブラウザ (`toro-chrome`) は別のイメージのコンテナで動かす。crime-buster 自身は Docker のソケットを持たず、Chromium も動かさない。

## 流れ

```
CoreProtect (TiDB) ── 3 秒ごとに新しい行 ──▶ 検知 (人の手では出せない速さか)
                                                │ 対象外: #tnt など人でない / TRUSTED_PLAYERS / WorldEdit などのコマンドを使った人 (review では対象外にせず、事件に書く)
                                                ▼
                                     事件を開く ── 90 秒 動きが止まる (または 10 分たつ) と閉じる
                                                ▼
   証拠の絵 ◀── 真上から見た「壊した場所」の図 (必ず作れる)
   画像 1   ◀── save-all flush → bluemap update → 描き終わるのを待つ → 3D の画面を撮る (被害)
   ロールバック ◀── co rollback u:<名前> t:<秒>s r:#<ワールド>  (効いたかはデータベースで確かめる)
   画像 2   ◀── もう 1 度 BlueMap を更新して撮る (復旧後)
                                                ▼
                         Discord の通報フォーラムに投稿 (荒らしの名前の投稿。同じ人の続きは書き足す)
```

荒らしが続けている間は、ロールバックしない (しても追いつかない)。止まってから、または 10 分ごとに、まとめて処理する。

## 動かし方 (CRIMEBUSTER_MODE)

| モード | することは | しないこと |
| --- | --- | --- |
| `observe` (既定) | 検知して、まとめ (`incident.json`) と真上の図を `DATA_DIR/reports/` に置く | コンソールへの送信、BlueMap の画面、Discord、ロールバック |
| `dry-run` | `observe` に加えて、`save-all flush` と `bluemap update` を送り、BlueMap の画面を撮り、報告文 (`report.md`) を作る。やる予定のロールバックのコマンドを報告文に書く | Discord に投稿しない、ロールバックしない |
| `review` | `dry-run` と同じに画像と報告文を作り、事件を「確認待ち (pending)」に置く。**人か AI (Muse など) が、コネクタ経由で、通報・ロールバック・却下・ミュート・キックを決める** | 自分では、投稿・ロールバック・キックをしない |
| `live` | すべて。ロールバック → 画像 → 投稿 (コネクタ経由の操作も受ける) | — |

**まず `observe` で数日、次に `dry-run` で 1 度画像を確かめてから、`review` (人や Muse が見て決める) か `live` にする。**

## 操作のためのコネクタ (control) と Muse

`CONTROL_ADDR` (既定 `:8090`) で、内部のネットワークに、コネクタを開く。人や AI が、事件を確かめて、通報・ロールバック・却下を決めるためのもので、[TORO-MCP](https://github.com/TORO-Server/TORO-MCP) (mcp.torosaba.net) が、**利用者のトークンをそのまま渡して**取り次ぐ。コネクタは、そのトークンを auth.torosaba.net に尋ね、**いまの Discord のロールが Admin 以上の人だけ**を通す。

```
Muse など ──(OAuth 2.0 のトークン)──▶ mcp.torosaba.net ──(同じトークン)──▶ crime-buster :8090 ──▶ CoreProtect / BlueMap / コンソール / Discord-MCP
```

| 経路 | 内容 |
| --- | --- |
| `GET /v1/status` | 動かし方、確認待ちの数、できること |
| `GET /v1/incidents?state=pending` `GET /v1/incidents/{id}` `GET /v1/incidents/{id}/images/{name}` | 事件の一覧・記録と報告文・証拠の画像 |
| `POST /v1/incidents/{id}/report` `…/rollback` `…/dismiss` | 通報 (1 事件に 1 回。却下した事件は 409) / ロールバック (裏で始まる 202。事件の始まりから 24 時間より古いものは 409) / 却下 |
| `POST /v1/players/{name}/mute` `…/kick` | ミュート (`CHAT_MUTE_CMD`) / キック。急ぐ操作なので、事件の処理の終わりを待たない。返すのは成否だけ (`{"ok":true}`。コンソールの行は返さない) |
| `GET /v1/players/{name}/activity` `GET /v1/chat` | 最近のブロックの変更・コマンド・発言 |
| `POST /v1/screenshot` `GET /v1/screenshots/{job}` | 指定した場所の BlueMap の撮影を始める (202 と job) / 撮れた画像 (まだなら 202)。撮るのに 1 分ほどかかり、Muse の 1 回の呼び出しは 45 秒で切れるので、2 回に分けている |
| `GET /v1/discord/tools` `POST /v1/discord/call` | 許可した Discord のツールだけ (`CONTROL_DISCORD_ALLOW`。読むことと、投稿・タグ付け。BAN・キック・ロール・削除は含めない)。チャンネルを指定するツールは、通報フォーラムとそのスレッドだけ。`discord_update_channel` はタグ (`applied_tags`) だけ。投稿の `allowed_mentions` は、呼ぶ側に関係なく「だれにも通知しない」にする |

- **認証は OAuth 2.0 の Bearer トークン** (共有の秘密は無い)。`AUTH_INTERNAL_URL` の `/introspect` に尋ね、返ってきた**いまの Discord のロール** (`roles_source` が `live`) が `ADMIN_ROLE_IDS` (既定 Owner と Admin) のどれかなら通す。トークンが無効 → 401、Admin ではない → 403、auth に尋ねられない・ロールを確かめられない → 503。**コネクタは公開しない** (ports を付けない、トンネルに載せない)。
- **scope**: トークンに `scope` が無ければ (本人が auth で発行した 90 日トークン) 通す。`scope` があるとき (OAuth の同意の範囲) は、空白で区切った中に `mcp` という言葉が要る。無ければ 403。読み取りだけに同意したトークンでは、管理の操作はできない。
- 頼んだ人は、トークンの持ち主 (Discord の ID と名前) で、操作 (GET 以外) はすべて `DATA_DIR/audit.jsonl` に 1 行ずつ残る。Discord の呼び出しは、ツール名・チャンネル・本文の SHA-256 も残す (本文そのものは残さない)。Discord の監査ログの理由は、頼んだ人の名前から始まる。
- 操作を受けるのは、`review` と `live` のときだけ。信頼する人と op には、ロールバック・ミュート・キックをしない。名前や ID は、英数字と `_ .` だけを通し、コンソールの命令にならないようにしている。データベースから来た名前とワールド名も同じに確かめ、形が正しくなければコンソールに送らない (事件の ID には、名前の代わりにユーザーの番号を使う)。
- 画像・発言・Discord の本文は、利用者が書いた文字なので、TORO-MCP が「信頼できない入力」と注意を付けて返す (AI が、中の指示に従わないように)。
- 事件の記録は `reports/<id>/record.json` (状態 `pending` / `reported` / `dismissed`、履歴、画像の名前)。

Muse に定期的に見てもらう手順は [docs/MUSE.md](docs/MUSE.md)。

## 検知

プレイヤーごとに、1 秒ごとの数を持って、次のどれかに当たると「荒らしの疑い」。

| 種類 | 既定 | 環境変数 |
| --- | --- | --- |
| 短い間に壊した数 | 5 秒に 100 個以上 | `BURST_WINDOW`, `BREAK_BURST` |
| 短い間に置いた数 | 5 秒に 150 個以上 | `PLACE_BURST` |
| 長い間に壊した数 | 10 分に 1,500 個以上 | `SUSTAINED_WINDOW`, `SUSTAINED_BREAKS` |

(人の手の上限は、1 秒に 4〜8 個ほど。2026-11-04 の fuuty119 は、1 秒に最大 421 個だった。)

次は、検知しても事件にしない (ログに「検知したが対象外」と出る)。

- 名前が `#` で始まる記録 (`#tnt`, `#fire`, `#wither`, `#water` など。人ではない)
- `TRUSTED_PLAYERS` に書いた人、`ops.json` (`OPS_FILE`) にいる op
- `//set`、`/fill`、`/clone`、`/setblock`、`/we`、`/br` などの編集のコマンドを、`WE_GRACE` (既定 30 分) 以内に使った人 (道具で速いのが当たり前)。ただし `review` では対象外にせず、事件の「検知」に「編集コマンドの使用あり」と書いて確認待ちに置く。記録されたコマンドの文字列を見るだけで、権限も成否も分からないので、人が確かめる。

## 通報

通報先は、フォーラム「荒らし通報」(チャンネル ID は環境変数 REPORT_CHANNEL_ID で指定)。このフォーラムの決まり (トピック: ワールド名・荒らしプレイヤーの名前・証拠画像 5 ブロック以上・画像 1〜2 枚) に合わせる。

- 投稿の名前は、荒らしの名前。本文に、ワールド、名前と UUID、座標、範囲、時間、数、主な破壊、使ったコマンド、対応を書く。
- 画像: 被害の全体 → (範囲が広いときは) いちばん壊された所に寄った画面 → (ロールバックできたら) 復旧後。BlueMap が撮れなかったときは、真上の図で埋める。
- BlueMap は「いまの地形」だけを描くので、穴や欠けだけでは、どこが荒らされたか分からない。そこで、撮った画面の上に印を重ねる: 壊された列を**赤い縦線**、主な範囲 (壊した数の 97%) を**黄色の枠**、左上に名前と壊した数。復旧後の画面は、同じカメラで、赤い線なしで撮る (前後を比べられる)。印の文字は ASCII だけ (実行する環境に日本語のフォントが無いため)。
- ロールバックできたら、タグ「ロルバ済」を付ける。
- 同じ人は 24 時間以内なら、新しく立てず、同じ投稿に書き足す。
- このフォーラムでは、ボットに「埋め込みのリンク」の権限が無いので、embed は使わず、普通の文字だけで書く。
- 誤検知のときに戻せるよう、報告に `co restore ...` のコマンドを書く。
- 投稿は `allowed_mentions` を空にして、だれにも通知しない。プレイヤーの書いた文字 (発言・コマンド・名前) と、確認した人のメモは、制御文字を除いて 1 行にし、`@` とインラインコードの区切りとマスクリンクを無効にしてから載せる。AI の `category` は決めた言葉だけにする。

### Discord の認証 (DiscordSRV のものを使う)

ボットのトークンは、DiscordSRV の `plugins/DiscordSRV/config.yml` の `BotToken` を、読み取り専用でマウントして読む。トークンを `.env` やリポジトリに写さない。
専用のボットを作るときは、`DISCORD_BOT_TOKEN_FILE` (Docker の secret など、ファイル 1 つ) か `DISCORD_BOT_TOKEN` で渡す。これがあれば DiscordSRV の設定は読まない。
REST だけを使い、Gateway には接続しない (DiscordSRV が同じトークンで接続しているので、もう 1 本つながない)。

### Discord MCP の経由で投稿する

Discord への投稿は、**Discord-MCP** (かふぇほらいずん公式のイメージ `ghcr.io/cafe-horizon/discord-mcp`) の MCP の経由で行う (`mcp.go`)。crime-buster は MCP のクライアントで、ステートレスな Streamable HTTP (`/mcp`) に `tools/call` を 1 回ずつ送る。

- サーバーは `compose.example.yaml` の `discord-mcp`。外には公開せず (ports なし)、`DISCORD_MCP_LAZY_TOOLS=true`、Gateway なし、`filePath`/`url` の添付なし、通報に要るツールだけ (`DISCORD_MCP_INCLUDE_TOOLS`)。
- サーバーにはトークンを置かない (`DISCORD_BOT_TOKEN=unused`)。crime-buster が DiscordSRV の BotToken を、呼ぶたびに `authOverride` (`Bot <token>`) で渡す。サーバーに `DISCORD_MCP_ALLOW_AUTH_OVERRIDE=true` が要る。
- 画像は Base64 (`files[].contentBase64`) で渡す (サーバーとディスクを共有しない)。
- 使うツール: `discord_get_my_user`、`discord_get_channel` (確認)、`discord_create_thread` (投稿を立てる)、`discord_create_message` (書き足す)、`discord_update_channel` (タグ)。
- 実際のイメージ (2025-11-25 の MCP の版) に、`tools/call` を送って、応答の形と `authOverride` が使えること (既定は「無効」と答える) を確かめた。

## AI (Claude) の確認

`AI_API_KEY` (または `ANTHROPIC_API_KEY`) を渡すと、Claude の Messages API (`AI_MODEL`、既定 `claude-haiku-5-5`) が、2 つの役をする。キーが無ければ使わない (これまでと同じ)。

1. **荒らしの確認 (ロールバックの前)**: 集計 (数・範囲・主な破壊・コマンド・コンテナ) と、画面 (赤い線つき) を見て、`荒らし` / `正当な作業` / `判断できない` と、確からしさ (0〜100) と一言を答える。`荒らし` で、`AI_MIN_CONFIDENCE` (既定 70) 以上のときだけ、ロールバックする。そうでないときは、ロールバックせず、通報だけして「運営が確認してください」と書く。AI が使えない (エラー) ときは、既定では閾値の決まりだけで進める。`AI_REQUIRED=true` なら、AI が「荒らし」と言うまでロールバックしない。
2. **チャットの監視**: 新しい発言を (最大 40 件を、1 人ずつ分けて) 見せて、暴言・性的な発言・差別・勧誘・スパムを選ぶ。回数の決まりでは見つからない発言を、通報にする。`AI_CHAT=false` で止められる。

安全の考え方: プレイヤーの名前・発言・コマンドは、AI には「データ」として渡し、命令には従わないように指示する。AI の答えが影響するのは「ロールバックを止める」と「通報する」方向だけで、AI の答えだけでロールバックやミュートが**増えることは無い** (ロールバックは、閾値を超えた事件にだけ行う。AI の判定だけで開いた事件は、`live` でもミュートせず通報だけにする。回数の決まりの検知が重なれば、ミュートする)。発言は 1 人ずつ尋ねるので、ほかの人の発言の文字が、その人の判定に影響しない。AI の答えの文章は、改行を除き、`@` を無効にして、長さを切ってから、報告に載せる。

> **送信されるもの**: 事件の集計と画面 (BlueMap の地図の画像)、チャットの発言 (名前つき) が、Anthropic の API に送られる。

## チャットのスパム

CoreProtect の `chat` の表 (`rowid, time, user, message`) を、ブロックと別のカーソルで見る。

- 連投: `CHAT_RATE` (既定 12) 件 / `CHAT_RATE_WINDOW` (10s)。カウントダウン「5 4 3 2 1 0 go」(8 件 / 10 秒) は、実際にあったので、誤検知しない値にした。
- 同じ発言: `CHAT_REPEAT` (5) 回 / `CHAT_REPEAT_WINDOW` (60s)。数字・記号・空白を除いて比べる (「買ってね 1」「買ってね 2」は同じ)。
- 同じ文字が `CHAT_RUN` (30) 個以上つづく発言。
- Discord の招待リンクを 5 分に 2 回、リンクつきの発言を `CHAT_LINK_REPEAT` (3) 回 / 5 分。
- 禁止語: `CHAT_BAD_WORDS_FILE` (1 行に 1 語、既定は使わない)。暴言・性的な発言は、回数では見つからないので、AI の確認 (上) か、この一覧で。
- 発言が止まって `CHAT_QUIET` (30s) たったら、報告する (発言の抜粋 12 行まで。`@` は無効にする)。報告は、ブロックの荒らしと同じフォーラムの、同じ人の投稿に書き足す。
- ミュートは `CHAT_MUTE_CMD` (例 `mute {name} 10m spam`) を設定したときだけ、`live` で行う。AI の判定だけの事件ではミュートしない。名前は Minecraft の名前の形 (英数字と `_` `.`、32 字まで) のときだけ使う (コマンドの注入を避ける)。付けるタグは `CHAT_MUTE_TAG_ID`。`toro-console` にも同じ `CHAT_MUTE_CMD` を渡す (中継の許可の形になる)。
- op と `TRUSTED_PLAYERS` は、対象外。
- 過去の記録で、誤検知を見積もる: `crime-buster scan-chat -days 365`。

> 2026-11-04 に確かめた: このボット (`TORO chat`) は、通報フォーラムを見られて、投稿・画像の添付ができる。「埋め込みのリンク」だけができない。

## BlueMap の画面

Chromium (`chromedp`) で、`BLUEMAP_URL` の地図を、荒らされた場所の中心 (壊した数の重み付きの中央値) にカメラを向けて開き、タイルが読み終わるまで待って撮る。

- **ブラウザは別のコンテナ** (`BROWSER_URL`、例 `ws://toro-chrome:9222`)。プレイヤーが置いた看板のある地図を開くので、crime-buster と同じ権限に置かない。1 枚ごとに新しいブラウザコンテキストを作り、撮ったら閉じる。画面の大きさは emulation で決める (フラグはブラウザのコンテナで固定)。
- ブラウザのコンテナは、BlueMap の名前を自分のフラグで toro-edge に向け、internal のネットワークだけに置く。外 (Cloudflare やインターネット) には出ない。あちらからの答えは信用しない (`/json/version` は接続先のパスだけを読む)。
- `BROWSER_URL` が空なら、ローカルの Chromium を起動する (開発用)。`CHROME_PATH`、`CHROME_FLAGS`、`BLUEMAP_RESOLVE` を見るのはこのときだけ。
- **看板は写さない。** `?marumasign3d=off` を付けて開くので、看板の処理は始まらず、プレイヤーの指定した画像や動画をブラウザが読まない。撮る直前に `window.marumasign3d.disable()` も呼ぶ (看板の処理が動いていても消す)。
- BlueMap の `update-cooldown` は 300 秒なので、撮る前に `save-all flush` と `bluemap update <地図> <x> <z> <半径>` を送り、`bluemap` の状態が「待機中・更新済み」になるのを 2 回続けて見るまで待つ (最大 6 分)。
- 描き直す範囲は、中心がカメラ (中央値)、半径は 256 ブロックまで。書き出しと更新は 1 分に 1 回まで (続けて撮るときは待つ)。プレイヤーの行動だけでサーバーの負荷が増えないようにしている。
- コンテナの中では GPU が無いので、ソフトウェアの WebGL (SwiftShader) で描く。1 枚あたり 30〜60 秒。
- 撮れなかったときは、画像なし (または真上の図だけ) で続ける。ロールバックも通報も止めない。

## コンソール

toro-main には shell が無く、RCON は使えない (Paper 26.3 と本番のプラグインで試すと、どのコマンドも `Cannot perform command async` で失敗する)。そこで Docker Engine の API (unix ソケット) で、`docker attach` と同じように標準入力へ 1 行送り、出力を読む。

ソケットは root と同じ力なので、crime-buster には持たせない。**同じイメージの `crime-buster console-relay` を、別のコンテナ `toro-console` で動かし、ソケットはそこだけにマウントする** (`relay.go`)。crime-buster とは、2 つだけの internal ネットワークでつなぐ (`CONSOLE_URL`、既定 `http://toro-console:8091`)。

```
POST /run {"cmd": "...", "timeout_ms": 1000}   200 のあと、コンソールの行を流す (timeout か、呼ぶ側が切るまで)
GET  /healthz                                  204
```

- つなぐのは `MC_CONTAINER` だけ。Docker の API は attach しか使わない。
- 送れるのは、文字列全体が次の形に一致するコマンドだけ。ほかは 403。名前は `validName` と同じ形、ワールドと地図は `[A-Za-z0-9_-]{1,64}`。
  - `save-all flush`
  - `bluemap`
  - `bluemap update <地図> <x> <z> <半径>` (半径は 1〜256)
  - `co rollback u:<名前> t:<秒>s r:#<ワールド>` (秒は 87000 まで)
  - `kick <名前> <理由>` (理由は制御文字を含まない 100 字まで)
  - `CHAT_MUTE_CMD` の `{name}` を正しい名前にしたもの
- 上限: 本文 4 KiB、同時に 4 本 (超えると 429)、`timeout_ms` は 1〜600000。
- 200 は「コマンドを送った」という意味。attach が途中で切れると、終わりの印を付けずに接続を切る。`RelayConsole` は、`done` を指定した呼び出しではこれをエラーにする (`io.ErrUnexpectedEOF`)。timeout は別のエラーになる。
- コンソールの行には、プレイヤーの発言も混ざる。完了の判定は、行の頭 (時刻とレベル) の直後の文言だけで見るので、発言と取り違えない。CoreProtect の完了の行は、対象の名前も照合する。
- **ロールバックが効いたかは、データベースで確かめる** (`rolled_back` が 95% 以上)。コンソールが「完了」でもデータベースで戻っていなければ失敗、切断でもデータベースで戻っていれば成功として記録する。データベースを読めないときだけ、コンソールの完了の行を信じる。
- コンソールの行は、コネクタの HTTP API の応答にも、事件の記録にも載せない (発言や、ログの設定によっては接続元のアドレスが混ざるため)。

`toro-console` は `read_only`、`cap_drop: ALL`、`no-new-privileges`、非 root で動かす。イメージにシェルが無いので、ヘルスチェックは `crime-buster healthcheck -relay`。

## データの保持とヘルスチェック

- `DATA_DIR/reports/<id>` は `REPORT_RETENTION` (既定 `2160h` = 90 日) で消す。起動したときと 1 時間ごとに見る。`0` なら消さない。動いている事件と、`reports/` と `shots/` の外には触らない。`audit.jsonl` は消さない。
- コネクタ経由の撮影の結果 (`shots/`) は 2 時間で消す。
- 1 件の事件の処理の全体の期限は 30 分。
- `crime-buster healthcheck` は、監視の 1 回が最後に成功した時刻で判定する (2 分より古ければ失敗)。データベースにつなげないあいだは unhealthy になる。

## 設定 (環境変数)

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `CRIMEBUSTER_MODE` | `observe` | `observe` / `dry-run` / `review` / `live` |
| `CONTROL_ADDR` | `:8090` (空 = コネクタを開かない) | コネクタの待ち受けアドレス |
| `CONTROL_DISCORD_ALLOW` | 読む・投稿・タグ付けだけ | コネクタから呼んでよい Discord のツール名の正規表現 |
| `DB_HOST` `DB_PORT` `DB_USER` `DB_PASSWORD` (`DB_PASSWORD_FILE`) `DB_NAME` `DB_TABLE_PREFIX` | `tidb` `4000` `root` (空) `coreprotect` `coreprotect_` | CoreProtect のデータベース (読むだけ)。`SELECT` だけの専用のユーザーにする (`root` のままだと、起動のログに警告が出る) |
| `BURST_WINDOW` `BREAK_BURST` `PLACE_BURST` `SUSTAINED_WINDOW` `SUSTAINED_BREAKS` | 5s 100 150 10m 1500 | 検知の閾値 |
| `QUIET` `MAX_INCIDENT` | 90s / 10m | 動きが止まって何秒で閉じるか / 長くても何分で閉じるか |
| `WE_GRACE` | 30m | 編集のコマンドを使って、この時間は検知しない |
| `TRUSTED_PLAYERS` `OPS_FILE` | (空) (空) | 検知しない人 (コンマ区切り) / op の一覧 (`ops.json`)。op も検知しない |
| `MAX_EVENT_AGE` | 15m | これより古い行は、見ても報告しない (止まっていたあいだのもの) |
| `DISCORD_BOT_TOKEN` (`DISCORD_BOT_TOKEN_FILE`) | (空) | ボットのトークン。あれば `DISCORDSRV_CONFIG` は読まない |
| `DISCORDSRV_CONFIG` | `/discordsrv/config.yml` | DiscordSRV の設定 (BotToken を読む) |
| `DISCORD_MCP_URL` `DISCORD_MCP_LAZY` `MCP_PROTOCOL_VERSION` | `http://discord-mcp:8085/mcp` `true` `2025-11-25` | Discord-MCP の場所 / `discord_call_tool` 1 本にまとめているか / MCP の版 |
| `AI_API_KEY` (`ANTHROPIC_API_KEY`、それぞれ `_FILE` も可) `AI_MODEL` `AI_BASE_URL` | (空 = AI を使わない) `claude-haiku-5-5` `https://api.anthropic.com` | AI の確認 |
| `AI_MIN_CONFIDENCE` `AI_REQUIRED` `AI_CHAT` | 70 `false` `true` | AI の判定を採る確からしさ / AI が「荒らし」と言わないとロールバックしない / チャットも AI に見せる |
| `CHAT_ENABLED` `CHAT_RATE` `CHAT_RATE_WINDOW` `CHAT_REPEAT` `CHAT_REPEAT_WINDOW` `CHAT_RUN` `CHAT_LINK_REPEAT` `CHAT_QUIET` | `true` 12 10s 5 60s 30 3 30s | チャットのスパムの閾値 |
| `CHAT_BAD_WORDS_FILE` `CHAT_MUTE_CMD` `CHAT_MUTE_TAG_ID` | (空) (空) (空) | 禁止語の一覧 / ミュートのコマンド (`{name}`) / ミュートしたときのタグ |
| `REPORT_CHANNEL_ID` `ROLLBACK_TAG_ID` | 通報フォーラム / 「ロルバ済」 | 通報先 |
| `CONSOLE_URL` | `http://toro-console:8091` | コンソールの中継の場所 |
| `KICK_ENABLED` | `false` | `live` で、検知したらすぐキックする |
| `BROWSER_URL` | (空 = ローカルの Chromium を起動する。開発用) | 画面を撮るブラウザのコンテナ (例 `ws://toro-chrome:9222`) |
| `BLUEMAP_URL` `MAP_IDS` | `http://3d-map.torosaba.net` `main=overworld` | 画面を撮る地図 / ワールド → 地図の名前 |
| `BLUEMAP_RESOLVE` `CHROME_PATH` `CHROME_FLAGS` | (空) (探す) (空) | ローカルの Chromium のときだけ: 名前の向け先 / Chromium の場所 / 追加のフラグ (コンマ区切り) |
| `SHOT_WIDTH` `SHOT_HEIGHT` `SHOT_SETTLE` | 1280 720 60s | 画像の大きさと、タイルを待つ最大の時間 |
| `WARPS_DIR` | (空) | EssentialsX の `warps/` (報告に「ワープ ○○ の近く」と書く) |
| `DATA_DIR` | `/data` | 状態 (`state.json`) と、報告のフォルダ |
| `REPORT_RETENTION` | `2160h` (90 日。`0` = 消さない) | 事件のフォルダを残す長さ |

`crime-buster console-relay` が読むもの (crime-buster の設定とは別):

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `CONSOLE_ADDR` | `:8091` | 中継の待ち受け |
| `DOCKER_SOCKET` | `/var/run/docker.sock` | Docker のソケット |
| `MC_CONTAINER` | `toro-main` | つなぐコンテナ |
| `CHAT_MUTE_CMD` | (空 = ミュートを通さない) | 許可するミュートのコマンドの形 (crime-buster と同じ値) |

## コマンド

```bash
crime-buster run                      # 監視を続ける
crime-buster console-relay            # コンソールの中継を開く (Docker のソケットを持つ別のコンテナで)
crime-buster healthcheck [-relay]     # 監視が回っているか / 中継が答えるか
crime-buster check [-console]         # データベース・Discord のトークンとチャンネルを確かめる (何も投稿しない)
crime-buster scan -days 30            # 過去の記録に、いまの閾値を当てはめて、誤検知を見積もる (何も書かない)
crime-buster scan-chat -days 365      # 同じことを、チャットで
crime-buster replay -user NAME -from 2026-11-04T10:50:00+09:00 -to 2026-11-04T11:00:00+09:00 -out DIR
                                    # 過去の 1 件に当てはめ、画像と報告文を DIR に作る (投稿もロールバックもしない)
```

## 本番に入れる手順

1. このリポジトリを `TORO-Server/CrimeBuster` に置き、タグ `v*` か main への push で、GHA が `ghcr.io/toro-server/crime-buster` を作る。
2. `compose.example.yaml` のサービス (`crime-buster`、`toro-console`、`toro-chrome`、`discord-mcp`) とネットワークを 本番の `docker-compose.yaml` に足し、`DOCKER_GID` と `crime-buster-data/` を用意する (GHCR のログインは他の非公開イメージと同じ)。
3. `CRIMEBUSTER_MODE=observe` で起動。`docker exec crime-buster /crime-buster check -console` (つながるか)。
4. 数日見て、検知の記録 (`docker logs crime-buster`) に誤検知が無いか確かめる。
5. `dry-run` にして、`/data/reports/` の画像と `report.md` を確かめる。
6. `review` にし、TORO-MCP に `CRIMEBUSTER_CONTROL_URL` を渡し、auth に `DISCORD_BOT_TOKEN` を設定する (いまのロールを調べるため)。Muse をつなぐ ([docs/MUSE.md](docs/MUSE.md))。人が見て決める運用で数件通す。
7. 自動でロールバックまでしたければ `live` にする (KICK を使うかも、ここで決める)。

## 確かめたこと (2026-11-04、本番のデータベースを読むだけで)

- **検知**: 過去 30 日の CoreProtect の記録 (約 733 万行) に当てはめて、通報の対象になったのは fuuty119 の 1 件 (10:52:38) だけ。編集のコマンドで対象外になったのが 192 件、人でないものが 20 件。
- **replay**: fuuty119 の記録から、真上の図・BlueMap の画像・報告文ができた。ソフトウェアの WebGL (GPU 無し) でも撮れた (約 60 秒)。
- **daemon**: `dry-run` で、過去の位置から監視を回し、1 件を検知 → 事件を閉じる → 画像と報告文を作る、までが動いた (投稿なし)。
- **Discord**: DiscordSRV のトークンで、通報フォーラムが見えることを確かめた (何も投稿していない)。
- 単体の試験 (`go test -race`): 検知、コンソールの読み取り、報告文、状態の保存など。

- **画面の印** (2026-11-04、再現): fuuty119 の記録で、赤い縦線が、壊された建物の部屋とピラミッドの上に載ることを目で確かめた (その後ロールバックされていたので、地図は復旧後の姿。印が、壊された場所を示す)。範囲が広いので、全体の画面と、寄った画面の 2 枚になる。
- **チャット**: 過去 1 年 (10,227 件) に当てはめて、通報の対象は 3 件 (同じ文字 127 個の連続、同じ発言の連投 2 件)。カウントダウンの誤検知は閾値を直して消した。
- **Discord-MCP**: 本物のサーバーに `tools/call` を送って、応答の形と、`authOverride` が「既定では無効」なことを確かめた。クライアントは、模擬のサーバーで試験した。

- **ブラウザのコンテナの形** (2026-11-05、ローカルの Chrome 156 の `--headless=new` で): `BROWSER_TEST_URL=ws://127.0.0.1:<port> go test -run TestShotOnRealBrowser` で、新しいブラウザコンテキストで撮れること、画像が指定の大きさになること、`?marumasign3d=off` と `disable()` が届くこと、撮ったあとにタブが残らないことを確かめた (ページは試験用のもので、本物の BlueMap ではない)。`chromedp/headless-shell` では、まだ確かめていない。

**まだ確かめていないこと**: AI (Claude) の実際の API (キーが無く、模擬のサーバーでしか試験していない)、DiscordSRV のトークンでの MCP 経由の実際の投稿、実際のロールバックのコマンドの返事 (CoreProtect の文言)、`bluemap update` を送ったあとの待ち、フォーラムへの投稿そのもの (タグ・画像の添付)、コンテナのイメージ (開発環境に Docker が無く、ビルドしていない)、本物の Docker と `toro-console` の組み合わせ (attach は模擬のサーバーでの試験だけ)、本物の BlueMap の画面での `toro-chrome` (別のリポジトリのイメージ)。`dry-run` と最初の `live` で確かめる。

## 限界

- ロールバックは、その人の変更を、ワールド全体 (`r:#<ワールド>`) で戻す。他の人が、荒らされた場所に置いた物も、その人の変更の上に重なっていると、CoreProtect の決まりで巻き込まれることがある。古い事件ほど巻き込むので、コネクタ経由と自動のロールバックは、事件の始まりから 24 時間までにしている。
- 道具 (WorldEdit など) を使う荒らしは、`review` 以外では対象外の決まりで見逃す (記録されたコマンドの文字列だけで決めるので、権限が無い人が打っただけでも対象外になる)。コマンドの記録が付くので、人が見られる。`review` では対象外にせず、事件に書く。
- CoreProtect が記録しないもの (モブの大量発生、パケットの攻撃など) は見られない。
- BlueMap が更新を終えられないとき (長い描画待ち) は、古い地図で撮ることがある (ログに警告が出る)。
