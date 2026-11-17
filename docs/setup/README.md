# 導入手順

上から順に進めます。所要時間は 1〜2 時間で、ほとんどがブラウザでの手作業です。

| # | やること | 詳しい手順 | 終わった目安 |
|---|---|---|---|
| 1 | 外部サービスを用意する（専用 Gmail、転送、カレンダーの共有、Google Cloud、OAuth、Discord、Claude API） | [準備](m0.md) | `go run ./cmd/m0check` がすべて ✓ |
| 2 | 振り分けの設定を書く | 下の「2. 設定を書く」 | `config.yaml` がある |
| 3 | Google Cloud にデプロイする | 下の「3. デプロイ」、[取り込みの仕組みと困ったとき](m1.md) | テストメールが Discord に届く |
| 4 | Discord のコマンドを試す | 下の「4. チャットを試す」 | `/status` に応答がある |

必要なツール: Go 1.26 以上、Git Bash（Windows の場合）、Google Cloud CLI。

---

## 1. 外部サービスを用意する

[準備](m0.md) の手順 0〜8 を進める。Discord の Bot を招待するときの Scopes `applications.commands` はスラッシュコマンドに必要なので忘れずに選ぶ。

## 2. 設定を書く

```bash
cp config.example.yaml config.yaml
```

最低限、次の2か所を自分に合わせて書き換える。他の値は初期値のままで動く。

- `jobhunt.profile`: 興味の傾向（例: 「自社開発の Web 系。Go やクラウドを使う企業。バックエンド志望」）。登録していない企業の興味度を LLM が推定するときに使う
- `jobhunt.companies`: 志望企業と志望度（S / A / B / C / none）。ここに書いた企業の説明会や連絡は優先して知らせる。後から Discord の `/ask` でも追加できる

`config.yaml` は志望企業などの個人情報を含むので Git にはコミットされない。Cloud Run には Secret Manager（`app-config`）経由で渡す。

## 3. デプロイ

```bash
export PROJECT_ID=<プロジェクトID>
./scripts/gcp-setup.sh       # API・Firestore・Pub/Sub・Secret Manager を用意する（何度実行してもよい）
./scripts/upload-secrets.sh  # .env・.secrets/・config.yaml を Secret Manager に登録する
./scripts/deploy.sh          # 本体・予算の監視・チャットをデプロイし、Discord にコマンドを登録する
```

`deploy.sh` が作るもの:

| 種類 | 名前 | 役割 |
|---|---|---|
| Cloud Run | `sheepbutler`（非公開） | メールの取り込み・判定・通知・`/ask` の回答 |
| Cloud Run | `sheepbutler-chat`（公開） | Discord のコマンドとボタンを受け取る。すべての要求で Discord の署名を確認する |
| Cloud Run | `sheepbutler-guard`（非公開） | 予算の通知を受け、超えそうなら本体を止める |
| Pub/Sub | `gmail-push`、`chat-jobs` | Gmail の新着通知、`/ask` の質問の受け渡し |
| Cloud Scheduler | `renew-gmail-watch`（毎日 4:00）、`sheepbutler-tick`（15 分おき） | Gmail の監視の張り直し、通知とまとめ報告 |
| 予算 | 月 100 円（`BUDGET_AMOUNT` で変更） | 50% で本体を一時停止、100% で課金を外す |

確認: 転送元のアドレスに「【重要】面接日程のご連絡（明日 12:00 締切）」のようなテストメールを送る。1 分ほどで Discord に即時通知が届けば成功。届かないときは [取り込みの仕組みと困ったとき](m1.md#うまくいかないとき) を参照。

設定を変えたら `upload-secrets.sh` と `deploy.sh` をもう一度実行する。

## 4. チャットを試す

通知用のチャンネルで次を試す（応答は自分にだけ見える）。

- `/status` — 選考状況
- `/ask 今週の締切を教えて` — 数秒〜数十秒で「考え中…」が回答に変わる

Bot の持ち主（Discord Developer Portal でアプリを作ったアカウント）以外は操作できない。

これで導入は完了です。日々の使い方は [使い方](../usage.md) を参照してください。

## 費用の目安

| サービス | 目安 |
|---|---|
| Google Cloud（Cloud Run・Firestore・Pub/Sub・Scheduler・Secret Manager） | 無料枠の範囲（月 0 円）。予算を超えそうなら自動で止まる |
| Claude API（Haiku 4.5） | メール 1 通 約 $0.002、`/ask` 1 回 約 $0.01。1 日の上限は `llm.daily_budget_usd`（既定 $0.5）。超えた日はルールだけで判定し、`/ask` は答えない |
| Discord | 無料 |
| LINE | 無料プランは月 200 通まで（`notify.line_monthly_limit` で調整） |

Claude API の費用は Google Cloud の予算とは別なので、Claude Console の Limits で月の上限も設定しておく。
