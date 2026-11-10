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

### 実ブラウザ試験の条件

実際のブラウザで撮影を確かめる試験 (`TestShotOnRealBrowser`) は、環境変数 `BROWSER_TEST_URL` (ブラウザの接続先) と `CHROME_TEST_PATH` (Chromium の場所) を設定したときだけ動く。設定しなければスキップされる。

## 設定リファレンスの照合

コードが読む環境変数の集合と、[configuration.md](configuration.md) の「このプログラムの設定」に書かれた名前の集合の一致と、廃止した用語が文書とコードに再混入していないことを、`scripts/check-docs.py` が検査する。CI の docs ジョブで実行される。ローカルでも実行できる。

```
python3 scripts/check-docs.py
```

Python の標準ライブラリだけを使う。環境変数を新しく読むようにしたときは、コード側の読み取りが `internal/config`、`internal/relay`、`internal/capture` のいずれかにあること (照合の対象はこの 3 ファイル) と、configuration.md への記載の両方が要る。

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
crime-buster console-relay            # コンソールの中継を開く (Docker のソケットを持つ別のコンテナで)
crime-buster healthcheck [-relay]     # 監視が回っているか / 中継が答えるか
crime-buster check [-console]         # データベース・Discord のトークンとチャンネルを確かめる (何も投稿しない)
crime-buster scan -days 30            # 過去の記録で検知の見積もり
crime-buster scan-chat -days 365      # 過去の記録でチャットの検知の見積もり
crime-buster replay ...               # 過去の 1 件の再現 (上の例)
```
