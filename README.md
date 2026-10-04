# SheepButler

メール・予定・就活の選考状況を整理し、必要度に応じて Discord か LINE で知らせる個人用の執事アプリ。
各自が自分の Google Cloud・OAuth クライアント・API キーを用意して動かすセルフホスト型です。

## 現在の状態

M1（メールの取り込み）まで実装済みです。手順は [M0 準備](docs/setup/m0.md)、[M1 メールの取り込み](docs/setup/m1.md) を参照してください。

| コマンド | 内容 |
|---|---|
| `go run ./cmd/authorize` | 専用 Gmail で OAuth の同意を行い、トークンを `.secrets/token.json` に保存する |
| `go run ./cmd/m0check` | Gmail・転送・カレンダー・Claude API・通知の接続を確認する |
| `scripts/gcp-setup.sh` | Google Cloud のリソース（API、Firestore、Pub/Sub、Secret Manager）を作る |
| `scripts/upload-secrets.sh` | ローカルの秘密情報を Secret Manager に登録する |
| `scripts/deploy.sh` | Cloud Run にデプロイし、Pub/Sub と watch 更新ジョブをつなぐ |
| `go run ./cmd/server -once sync` | ローカルから1回だけ同期する（確認用） |

## 構成

```
cmd/server          Cloud Run で動く本体（Pub/Sub の受信、watch の更新）
cmd/authorize       OAuth の認可（ローカルで1回だけ実行）
cmd/m0check         M0 の接続確認
internal/envfile    .env の読み込み
internal/googleauth Google の OAuth 設定とトークン（読み取り専用スコープのみ）
internal/notify     Discord / LINE への送信
internal/ingest     Gmail の差分取得と二重処理の防止
internal/mail       Gmail API（読み取り専用）
internal/store      Firestore への保存
internal/model      保存するデータの型
internal/logx       Cloud Logging 向けのログ
scripts/            Google Cloud のセットアップ
docs/setup/         セットアップ手順
```

秘密情報（`.env`、`.secrets/`）は Git にコミットされません。
