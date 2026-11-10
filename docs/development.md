# 開発

## ビルド

```
go build ./cmd/crime-buster
```

イメージは Dockerfile で作る (実行ファイルと CA 証明書だけの FROM scratch、非 root)。

## テスト

```
go test ./...
```

CI では、gofmt の検査、`go vet ./...`、`go test -race ./...`、govulncheck を実行する。

撮影の試験は、Renderer の API の試験用サーバー (httptest) で行う。実物の Renderer は要らない。

## 設定リファレンスの照合

コードが読む環境変数の集合と、[configuration.md](configuration.md) の「このプログラムの設定」に書かれた名前の集合の一致と、廃止した用語が文書とコードに再混入していないことを、`scripts/check-docs.py` が検査する。CI の docs ジョブで実行される。ローカルでも実行できる。

```
python3 scripts/check-docs.py
```

Python の標準ライブラリだけを使う。環境変数を新しく読むようにしたときは、コード側の読み取りが照合の対象のファイル (`internal/config/config.go` と `internal/capture/shot.go`) にあることと、configuration.md への記載の両方が要る。

## 過去の記録で見積もりを取る

どちらも何も書かず、投稿もロールバックもしない。

```bash
crime-buster scan -days 30            # 過去のブロックの記録に、いまの閾値を当てはめて、誤検知を見積もる
crime-buster scan-chat -days 365      # 同じことを、チャットで
crime-buster replay -user NAME -from 2026-11-04T10:50:00+09:00 -to 2026-11-04T11:00:00+09:00 -out DIR
                                    # 過去の 1 件に当てはめ、画像と報告文を DIR に作る
```

## コマンド一覧

```bash
crime-buster run                      # 監視を続ける
crime-buster healthcheck              # 監視が回っているか
crime-buster check [-console]         # データベース・Discord のトークンとチャンネルを確かめる (何も投稿しない)。-console は RCON の疎通も確かめる
crime-buster scan -days 30            # 過去の記録で検知の見積もり
crime-buster scan-chat -days 365      # 過去の記録でチャットの検知の見積もり
crime-buster replay ...               # 過去の 1 件の再現 (上の例)
```
