#!/usr/bin/env bash
# SheepButler を Cloud Run にデプロイし、Gmail の新着通知と watch 更新ジョブをつなぐ。
# 何度実行しても壊れない（既存のリソースは更新する）。
#
#   PROJECT_ID=your-project ./scripts/deploy.sh
#
# 前提: scripts/gcp-setup.sh と scripts/upload-secrets.sh を実行済みであること。
set -euo pipefail

: "${PROJECT_ID:?PROJECT_ID を指定してください}"
REGION="${REGION:-asia-northeast1}"
SERVICE="${SERVICE:-sheepbutler}"
TOPIC="${TOPIC:-gmail-push}"
SUBSCRIPTION="${SUBSCRIPTION:-gmail-push-sub}"
JOB="${JOB:-renew-gmail-watch}"
TICK_JOB="${TICK_JOB:-sheepbutler-tick}"
RUN_SA="sheepbutler-run@$PROJECT_ID.iam.gserviceaccount.com"
# Pub/Sub と Cloud Scheduler が Cloud Run を呼ぶときに名乗るアカウント
INVOKER_NAME="sheepbutler-invoker"
INVOKER_SA="$INVOKER_NAME@$PROJECT_ID.iam.gserviceaccount.com"

cd "$(dirname "$0")/.."
gcloud config set project "$PROJECT_ID" >/dev/null

echo "== 呼び出し用サービスアカウント"
if ! gcloud iam service-accounts describe "$INVOKER_SA" >/dev/null 2>&1; then
  gcloud iam service-accounts create "$INVOKER_NAME" --display-name="SheepButler invoker (Pub/Sub, Scheduler)"
fi

# 転送元アカウントの一覧は .env の MAIL_SOURCES から読む（例: 大学=a@ms.dendai.ac.jp;個人=b@gmail.com）。
MAIL_SOURCES="${MAIL_SOURCES:-$(grep -E '^MAIL_SOURCES=' .env 2>/dev/null | head -1 | cut -d= -f2- || true)}"
: "${MAIL_SOURCES:?.env に MAIL_SOURCES を設定してください}"
case "$MAIL_SOURCES" in *,*) echo "MAIL_SOURCES の区切りは ; です（, は使えません）"; exit 1 ;; esac

# .env から1つの値を読む（環境変数で指定されていればそちらを優先）。
env_value() { local v="${!1:-}"; [ -n "$v" ] && { echo "$v"; return; }; grep -E "^$1=" .env 2>/dev/null | head -1 | cut -d= -f2- || true; }
has_secret() { gcloud secrets versions describe latest --secret="$1" >/dev/null 2>&1; }

# 通知先とカレンダー（秘密ではない値は環境変数で渡す）
NOTIFY_CHANNEL=$(env_value NOTIFY_CHANNEL)
ENV_ARG="GOOGLE_CLOUD_PROJECT=$PROJECT_ID,GMAIL_PUBSUB_TOPIC=projects/$PROJECT_ID/topics/$TOPIC,RETENTION_DAYS=${RETENTION_DAYS:-90},MAIL_SOURCES=$MAIL_SOURCES,CONFIG_PATH=/config/config.yaml"
ENV_ARG="$ENV_ARG,NOTIFY_CHANNEL=${NOTIFY_CHANNEL:-discord},DISCORD_CHANNEL_ID=$(env_value DISCORD_CHANNEL_ID),LINE_USER_ID=$(env_value LINE_USER_ID),GOOGLE_CALENDAR_ID=$(env_value GOOGLE_CALENDAR_ID)"

# 秘密情報。config.yaml（app-config）は登録されていればファイルとしてマウントし、無ければ初期設定で動かす。
SECRETS_ARG="GOOGLE_OAUTH_CLIENT_JSON=google-oauth-client:latest,GOOGLE_OAUTH_TOKEN_JSON=google-oauth-token:latest,ANTHROPIC_API_KEY=anthropic-api-key:latest"
if has_secret app-config; then
  SECRETS_ARG="$SECRETS_ARG,/config/config.yaml=app-config:latest"
else
  echo "   config.yaml が未登録のため、判定は初期設定で動かす（scripts/upload-secrets.sh で登録できる）"
fi
has_secret discord-bot-token && SECRETS_ARG="$SECRETS_ARG,DISCORD_BOT_TOKEN=discord-bot-token:latest"
has_secret line-channel-access-token && SECRETS_ARG="$SECRETS_ARG,LINE_CHANNEL_ACCESS_TOKEN=line-channel-access-token:latest"

echo "== Cloud Run にデプロイ（ソースからビルド）"
# Windows の Git Bash は "=/config/..." のような値をパスとみなして書き換えるため、環境変数と秘密情報の引数は変換しない。
MSYS2_ARG_CONV_EXCL="--set-env-vars=;--set-secrets=" gcloud run deploy "$SERVICE" \
  --source . \
  --region "$REGION" \
  --service-account "$RUN_SA" \
  --no-allow-unauthenticated \
  --max-instances 1 \
  --memory 256Mi \
  --set-env-vars="$ENV_ARG" \
  --set-secrets="$SECRETS_ARG" \
  --timeout 300 \
  --quiet

URL=$(gcloud run services describe "$SERVICE" --region "$REGION" --format='value(status.url)')
echo "   URL: $URL"

# 呼び出し用アカウントにだけ Cloud Run の呼び出しを許可する（外部からは呼べない）。
gcloud run services add-iam-policy-binding "$SERVICE" --region "$REGION" \
  --member="serviceAccount:$INVOKER_SA" --role=roles/run.invoker >/dev/null

echo "== Pub/Sub の push サブスクリプション"
PUSH_ARGS=(
  --push-endpoint="$URL/pubsub/gmail"
  --push-auth-service-account="$INVOKER_SA"
  --push-auth-token-audience="$URL"
  --ack-deadline=60
  --min-retry-delay=10s
  --max-retry-delay=600s
)
if gcloud pubsub subscriptions describe "$SUBSCRIPTION" >/dev/null 2>&1; then
  gcloud pubsub subscriptions update "$SUBSCRIPTION" "${PUSH_ARGS[@]}" >/dev/null
else
  gcloud pubsub subscriptions create "$SUBSCRIPTION" --topic="$TOPIC" "${PUSH_ARGS[@]}" >/dev/null
fi

echo "== watch を毎日張り直すジョブ（毎日 4:00 JST）"
JOB_ARGS=(
  --location="$REGION"
  --schedule="0 4 * * *"
  --time-zone="Asia/Tokyo"
  --uri="$URL/jobs/renew-watch"
  --http-method=POST
  --oidc-service-account-email="$INVOKER_SA"
  --oidc-token-audience="$URL"
)
if gcloud scheduler jobs describe "$JOB" --location="$REGION" >/dev/null 2>&1; then
  gcloud scheduler jobs update http "$JOB" "${JOB_ARGS[@]}" >/dev/null
else
  gcloud scheduler jobs create http "$JOB" "${JOB_ARGS[@]}" >/dev/null
fi
# ガードが一時停止していた場合に備えて再開する（既に有効なら何もしない）。
gcloud scheduler jobs resume "$JOB" --location="$REGION" >/dev/null 2>&1 || true

echo "== 通知の処理を15分おきに行うジョブ"
TICK_ARGS=(
  --location="$REGION"
  --schedule="*/15 * * * *"
  --time-zone="Asia/Tokyo"
  --uri="$URL/jobs/tick"
  --http-method=POST
  --oidc-service-account-email="$INVOKER_SA"
  --oidc-token-audience="$URL"
  --attempt-deadline=180s
)
if gcloud scheduler jobs describe "$TICK_JOB" --location="$REGION" >/dev/null 2>&1; then
  gcloud scheduler jobs update http "$TICK_JOB" "${TICK_ARGS[@]}" >/dev/null
else
  gcloud scheduler jobs create http "$TICK_JOB" "${TICK_ARGS[@]}" >/dev/null
fi
gcloud scheduler jobs resume "$TICK_JOB" --location="$REGION" >/dev/null 2>&1 || true

echo "== watch を今すぐ登録"
gcloud scheduler jobs run "$JOB" --location="$REGION"

# デプロイのたびにイメージが溜まると Artifact Registry の無料枠（0.5GB）を超えるため、最新2つだけ残す。
echo "== 古いイメージの自動削除"
POLICY=$(mktemp)
cat > "$POLICY" <<'JSON'
[
  {"name": "keep-recent", "action": {"type": "Keep"}, "mostRecentVersions": {"keepCount": 2}},
  {"name": "delete-others", "action": {"type": "Delete"}, "condition": {"tagState": "ANY"}}
]
JSON
gcloud artifacts repositories set-cleanup-policies cloud-run-source-deploy \
  --location="$REGION" --policy="$POLICY" --no-dry-run >/dev/null
rm -f "$POLICY"

# 予算の監視と自動停止（ガード）も同時に更新する。
./scripts/deploy-guard.sh

cat <<EOF

デプロイしました。動作確認:
  1. 大学アドレスにテストメールを送る
  2. 1分ほど待ってログを確認する
     gcloud run services logs read $SERVICE --region $REGION --limit 20
  3. 「同期しました」のログで created が 1 以上になっていれば成功
EOF
