# 変更履歴

版と変更の対応。タグは版と対応する。

## v0.4.0

- 撮影を BlueMap のブラウザ撮影から TORO-Renderer の描画 API へ替えた。ブラウザ (Chromium、chromedp) は持たなくなった。印 (壊された列の線、範囲の枠、ラベル) は、Renderer が返すカメラの変換を使ってこちらで描く。地図の更新待ち (`bluemap update`) は無くなり、撮る前の `save-all flush` だけを残した
- コンソールの転送を、Docker のソケットを持つ別プロセス (中継、`console-relay`) から、Paper 標準の RCON へ替えた。crime-buster は Docker のソケットを持たなくなった。送れるコマンドの許可リストは、送る側で送る前に確かめる形にし、通す形は変えていない (BlueMap の形だけを除いた)。RCON の応答はコマンドへの返事だけなので、ロールバックの成否は従来どおりデータベースで確かめる
- 設定の変更: `CONSOLE_URL`、`BLUEMAP_URL`、`BROWSER_URL`、`BLUEMAP_RESOLVE`、`CHROME_PATH`、`CHROME_FLAGS`、`SHOT_SETTLE` と、中継の設定 (`CONSOLE_ADDR`、`DOCKER_SOCKET`、`MC_CONTAINER`) を廃止し、`RCON_ADDR`、`RCON_PASSWORD`、`RENDERER_URL` を足した。`MAP_IDS` の対応先は Renderer のマップ名になった。コンソールを使うモード (review、live) では `RCON_ADDR` と `RCON_PASSWORD` が必須になった
- `console-relay` サブコマンドと `healthcheck -relay` を廃止した。`check -console` は RCON の疎通の確認になった

## v0.3.0

- コネクタの環境変数の旧名 (`CONTROL_ADDR`、`CONTROL_REQUIRE_LIVE_ROLES`、`CONTROL_DISCORD_ALLOW`) の互換を廃止した。新しい名前 (`CONNECTOR_`) のみを読む

## v0.2.1

- 状態の応答の `version` が常に `dev` になっていたのを直した (CI がイメージのビルドへ版を渡していなかった)

## v0.2.0

- 設定を用途別のグループに分け、検証を `Validate` に集約した
- 事件の状態に型を付け、遷移を `Transition` の 1 箇所に集約した
- モードの能力を `CapabilitiesFor` に一元化した
- パッケージを `cmd/crime-buster` と `internal` の各パッケージに分割した
- 依存をインターフェース (ポート) 越しにし、`Guard` の責務を事件処理・チャット処理・保持期間に分割した
- コネクタの識別子を connector に統一した (環境変数の旧名は、互換として受理する)
- 文書を役割ごとに分割し、設定リファレンスとコードの機械照合を CI に加えた
- 依存を更新した (chromedp 0.23.0、cdproto 0.159.2 など)

## v0.1.3

- このリポジトリでの最初の公開。プログラムの内容は v0.1.2 を引き継ぐ
- Discord の ID の固定値を、コードの既定値と設定例から除いた。未設定のときは、通報は外部へ試みずに止まり、コネクタはどのロールでも通さない (安全な側に倒す)

## v0.1.2

- 名称を CrimeBuster に統一した版

## v0.1.1

- 初期の公開版

## v0.1.0

- 初期の公開版
