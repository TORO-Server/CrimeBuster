# 設定リファレンス

設定は環境変数で渡す。読み取りと検証はコード側 (`internal/config`) が正本であり、検証 (`Validate`) は起動時に全項目を確かめ、誤りは一括で報告する。この文書の「このプログラムの設定」の表に書かれた名前の集合と、コードが読む名前の集合が一致することは、CI が機械照合している (`scripts/check-docs.py`)。

秘密の値は、環境変数の代わりにファイルで渡せるものがある (名前の末尾に `_FILE` を付けた変数にファイルのパスを渡す。Docker の secret など)。

<!-- config-check:begin -->

## このプログラムの設定

### モード

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `CRIMEBUSTER_MODE` | `observe` | 動かし方。`observe` / `dry-run` / `review` / `live`。モードと能力の対応は [architecture.md](architecture.md) |

### コネクタ

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `CONNECTOR_ADDR` | `:8090` | コネクタの待ち受けアドレス。空ならコネクタを開かない |
| `AUTH_INTERNAL_URL` | `http://toro-auth:8081` | 認証サービスの introspection の接続先 (内部のネットワークだけ) |
| `ADMIN_ROLE_IDS` | (無し。環境変数で指定) | コネクタを使える Discord のロールの ID (コンマ区切り)。指定が無ければ、どのロールでも通さない。操作を受けるモードでは必須 |
| `CONNECTOR_REQUIRE_LIVE_ROLES` | `true` | 認可で、いまの Discord のロール (roles_source が live のもの) だけを信じるか。 |
| `CONNECTOR_DISCORD_ALLOW` | 読む・投稿・タグ付けだけを許可する正規表現 | コネクタから呼んでよい Discord のツール名の正規表現 |

### データベース

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `DB_HOST` | `tidb` | CoreProtect のデータベースのホスト |
| `DB_PORT` | `4000` | ポート |
| `DB_USER` | `root` | ユーザー。読むだけの専用ユーザーにするのがよい (既定のままだと起動のログに警告が出る) |
| `DB_PASSWORD` | (空) | パスワード。ファイルで渡すときは `DB_PASSWORD_FILE` |
| `DB_NAME` | `coreprotect` | データベース名 |
| `DB_TABLE_PREFIX` | `coreprotect_` | テーブルの接頭辞 |

### 検知

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `BURST_WINDOW` | `5s` | 短い窓の長さ |
| `BREAK_BURST` | `100` | 短い窓のうちに、これ以上壊したら検知 |
| `PLACE_BURST` | `150` | 短い窓のうちに、これ以上置いたら検知 |
| `SUSTAINED_WINDOW` | `10m` | 長い窓の長さ |
| `SUSTAINED_BREAKS` | `1500` | 長い窓のうちに、これ以上壊したら検知 |
| `QUIET` | `90s` | 動きが止まって、この時間たったら事件を閉じる |
| `MAX_INCIDENT` | `10m` | 事件の長さの上限 |
| `WE_GRACE` | `30m` | 編集のコマンドを使って、この時間は検知しない |
| `TRUSTED_PLAYERS` | (空) | 検知しない人 (コンマ区切り) |
| `OPS_FILE` | (空) | op の一覧のファイル。op も検知しない |
| `POLL_INTERVAL` | `3s` | データベースに新しい行を尋ねる間隔 |
| `MAX_EVENT_AGE` | `15m` | これより古い行は、見ても報告しない |

### チャット

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `CHAT_ENABLED` | `true` | チャットの検知をするか |
| `CHAT_RATE` | `12` | 連投の件数の閾値 |
| `CHAT_RATE_WINDOW` | `10s` | 連投の窓 |
| `CHAT_REPEAT` | `5` | 同じ発言の回数の閾値 |
| `CHAT_REPEAT_WINDOW` | `60s` | 同じ発言の窓 |
| `CHAT_RUN` | `30` | 同じ文字がこれ以上つづく発言を検知 |
| `CHAT_LINK_REPEAT` | `3` | リンクつきの発言の回数の閾値 (5 分のうち) |
| `CHAT_BAD_WORDS_FILE` | (空) | 禁止語の一覧のファイル (1 行に 1 語)。空なら使わない |
| `CHAT_QUIET` | `30s` | 発言が止まって、この時間たったら報告する |
| `CHAT_MUTE_CMD` | (空) | ミュートのコマンド (`{name}` を名前に置き換える)。空ならミュートしない |
| `CHAT_MUTE_TAG_ID` | (空) | ミュートしたときに投稿へ付けるタグ。空なら付けない |

### AI

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `AI_API_KEY` | (空) | AI の API キー。ファイルで渡すときは `AI_API_KEY_FILE`。空なら AI は使わない |
| `ANTHROPIC_API_KEY` | (空) | キーの別名。`AI_API_KEY` が空のときに見る。ファイルで渡すときは `ANTHROPIC_API_KEY_FILE` |
| `AI_BASE_URL` | `https://api.anthropic.com` | AI の API の接続先 |
| `AI_MODEL` | `claude-haiku-5-5` | 使うモデル |
| `AI_MIN_CONFIDENCE` | `70` | AI の判定を採る確からしさ (0〜100) の下限 |
| `AI_REQUIRED` | `false` | true なら、AI が「荒らし」と答えないかぎりロールバックしない |
| `AI_CHAT` | `true` | チャットの発言も AI に見せるか |

### Discord と投稿先

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `DISCORD_BOT_TOKEN` | (空) | ボットのトークン。ファイルで渡すときは `DISCORD_BOT_TOKEN_FILE`。あれば DiscordSRV の設定は読まない |
| `DISCORDSRV_CONFIG` | `/discordsrv/config.yml` | DiscordSRV の設定ファイル。ボットのトークンをここから読む |
| `REPORT_CHANNEL_ID` | (無し。環境変数で指定) | 通報先フォーラムのチャンネル ID。投稿するモードでは必須 |
| `ROLLBACK_TAG_ID` | (無し。環境変数で指定) | ロールバックできたときに付けるタグの ID |
| `DISCORD_MCP_URL` | `http://discord-mcp:8085/mcp` | Discord-MCP の接続先 |
| `DISCORD_MCP_LAZY` | `true` | Discord-MCP がツールを 1 本にまとめているか |
| `MCP_PROTOCOL_VERSION` | `2025-11-25` | MCP の版 |

### コンソール

コンソールへは Paper 標準の RCON で直接送る。送れるコマンドは、送る前に許可リストで確かめる (許可する形は architecture を参照)。
コンソールを使うモード (操作を受ける review、自動の live) では、`RCON_ADDR` と `RCON_PASSWORD` が必須である。

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `RCON_ADDR` | (空) | RCON の接続先 (`host:port`)。空ならコンソールを使わない |
| `RCON_PASSWORD` | (空) | RCON のパスワード。ホスト側の env_file で渡す (`RCON_PASSWORD_FILE` でファイルからも読める) |
| `KICK_ENABLED` | `false` | live で、検知したらすぐキックするか |

### 撮影

撮影は TORO-Renderer の描画 API に頼む。Renderer は region ファイルを直接読むので、地図の更新を待つ必要はない。

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `RENDERER_URL` | (空) | Renderer の API の接続先 (例 `http://toro-renderer:8080`) |
| `MAP_IDS` | `main=overworld` | ワールド名と Renderer のマップ名の対応 (コンマ区切り) |
| `SHOT_WIDTH` | `1280` | 画像の幅 |
| `SHOT_HEIGHT` | `720` | 画像の高さ |

### 保存先と保持

| 名前 | 既定 | 意味 |
| --- | --- | --- |
| `WARPS_DIR` | (空) | ワープの一覧のフォルダ。報告に「ワープ ○○ の近く」と書くために読む |
| `DATA_DIR` | `/data` | 状態と報告のフォルダ |
| `REPORT_RETENTION` | `2160h` (90 日) | 事件のフォルダを残す長さ。`0` なら消さない |

<!-- config-check:end -->

## 他システムの設定 (このプログラムは読まない)

次の名前は、このプログラムの設定ではない。混同しないよう、所有者を明記して分けてある。これらは CI の照合の対象外である。

### Discord-MCP サーバー側の設定

| 名前 | 意味 |
| --- | --- |
| `DISCORD_MCP_ALLOW_AUTH_OVERRIDE` | 呼ぶ側がトークンを渡すことを許可するか |
| `DISCORD_MCP_ALLOW_FILE_PATH` | ファイルのパスによる添付を許可するか |
| `DISCORD_MCP_ALLOW_FILE_URL` | URL による添付を許可するか |
| `DISCORD_MCP_ENABLE_GATEWAY` | Discord の Gateway に接続するか |
| `DISCORD_MCP_INCLUDE_TOOLS` | 登録するツールの範囲 |
| `DISCORD_MCP_LAZY_TOOLS` | ツールを 1 本にまとめるか (本体の `DISCORD_MCP_LAZY` と対にする) |
| `MCP_TRANSPORT` | MCP の通信方式 |
| `MCP_HTTP_PORT` | MCP サーバーの待ち受けポート |
| `MCP_ALLOWED_HOSTS` | 許可するホスト |

### 連携先の MCP サーバーの設定

| 名前 | 意味 |
| --- | --- |
| `CRIMEBUSTER_CONNECTOR_URL` | コネクタの接続先。連携先の MCP サーバーにこれを渡すと、管理用のツールが有効になる |


