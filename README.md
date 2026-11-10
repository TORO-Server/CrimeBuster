# CrimeBuster — 荒らしを見つけて、証拠の画像を付けて通報し、ロールバックする

Minecraft サーバーの CoreProtect の記録を監視し、人の手では出せない速さでブロックを壊したり置いたりしている人を見つけたら、荒らされた場所の画像 (BlueMap の 3D の地図) を撮り、その人のブロックの変更を CoreProtect でロールバックし、Discord のフォーラムに証拠の画像つきで報告するプログラム。言語は Go で、実行ファイルは 1 つ。イメージの中身は、実行ファイルと CA 証明書だけ (FROM scratch、非 root)。

動かし方は 4 つ (モード) あり、検知して記録するだけの `observe` から、投稿とロールバックまで自動で行う `live` まで、段階を上げて使う。人か AI が確認してから決める運用では `review` を使い、事件は「確認待ち」になり、コネクタの HTTP API 経由で通報・ロールバック・却下を決める。

## クイックスタート

1. `compose.example.yaml` のサービス定義を、自分の docker-compose.yaml に取り込む。データ用のディレクトリと、データベースのパスワードの secret を用意する。
2. 環境変数 `CRIMEBUSTER_MODE=observe` で起動する。このモードは、検知して記録するだけで、外へは何もしない。
3. `crime-buster check` で、データベースと Discord の接続を確かめる (何も投稿しない)。
4. しばらく動かして検知の記録を見て、誤検知が無いことを確かめてから、`dry-run`、`review`、`live` の順に段階を上げる。

導入の手順の詳細は [docs/operations.md](docs/operations.md)、設定は [docs/configuration.md](docs/configuration.md) にある。

## 文書

| 文書 | 内容 |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | 構成要素と依存の方向、用語集、モードと能力、事件の状態と遷移 |
| [docs/configuration.md](docs/configuration.md) | 設定リファレンス (環境変数)。コードの設定定義と CI で機械照合している |
| [docs/detection.md](docs/detection.md) | 検知の仕様 (ブロック変更、チャット) |
| [docs/connector-api.md](docs/connector-api.md) | コネクタの HTTP API の仕様 (経路、認証、認可、監査) と、コネクタ経由で操作する手順 |
| [docs/reporting.md](docs/reporting.md) | 通報の仕様 (Discord への投稿、MCP 経由の投稿、AI 判定の位置付け) |
| [docs/console-relay.md](docs/console-relay.md) | コンソール中継のプロトコル |
| [docs/operations.md](docs/operations.md) | 導入手順、データの保持期間、ヘルスチェック、限界 |
| [docs/development.md](docs/development.md) | ビルド、テスト、実ブラウザ試験の条件、過去の記録で見積もりを取るコマンド |
| [docs/verification/](docs/verification/) | 日付付きの検証記録 (履歴。規範ではない) |
| [CHANGELOG.md](CHANGELOG.md) | 版と変更の対応 |

## 変更ガイド

何を変えたいときに、どこを触るかの一覧。設定リファレンスと用語は、転載せずに正本を参照する形にしている。

| 何を変えたい | 触るコード | 触る文書 |
| --- | --- | --- |
| 検知の閾値の既定値 | `internal/config` の検知グループの定義 | なし (設定リファレンスは CI が照合する) |
| 検知の条件そのもの | `internal/detect` | `docs/detection.md` |
| モードの能力 (何をしてよいか) | `internal/config` の `CapabilitiesFor` | `docs/architecture.md` の能力の表 |
| 事件の状態と遷移 | `internal/domain` の状態定義と `Transition` | `docs/architecture.md` の状態の節 |
| コネクタの経路 | `internal/connector` | `docs/connector-api.md` |
| 認可の条件 | `internal/connector` の認可 | `docs/connector-api.md` の認可の節 |
| 通報文の形式 | `internal/notify` | `docs/reporting.md` |
| コンソール中継で許可するコマンド | `internal/relay` | `docs/console-relay.md` |
| データの保持期間 | `internal/config` の保持グループと `internal/app` の保持処理 | `docs/operations.md` |
