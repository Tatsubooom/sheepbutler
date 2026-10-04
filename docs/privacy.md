# プライバシーポリシー

SheepButler は、利用者が自分の Google Cloud プロジェクトで動かす個人用のアプリケーションです。
開発者を含む第三者のサーバーにデータを送ることはありません。

## 取得する情報

Google アカウントについて、次の読み取り専用の権限だけを使います。

| 権限 | 用途 |
|---|---|
| `gmail.readonly` | 受信したメールの差出人・宛先・件名・冒頭の抜粋を読み、必要度を判定する |
| `calendar.readonly` | 予定を読み、まとめ報告に含める |

メールの送信・削除・変更、予定の作成・変更は行いません。

## 保存と送信

- 取得した情報は、利用者自身の Google Cloud プロジェクト（Firestore）に保存します。メール本文は保存しません。
- 保存したメールの情報は、受信から一定期間（既定 90 日）で自動的に削除されます。
- 必要度の判定のため、メールの件名と冒頭の一部を、利用者が設定した LLM の API（Anthropic の Claude API）に送ります。
- 通知は、利用者が設定した Discord または LINE に送ります。

## Google ユーザーデータの扱い

SheepButler による Google API から受け取った情報の使用と他のアプリへの転送は、[Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy)（Limited Use の要件を含む）に従います。
取得した情報を広告に使ったり、販売したりすることはありません。

## 権限の取り消し

[Google アカウントのサードパーティ製アプリの管理画面](https://myaccount.google.com/connections)から、いつでも SheepButler の権限を取り消せます。
