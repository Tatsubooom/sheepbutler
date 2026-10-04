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

echo "== Cloud Run にデプロイ（ソースからビルド）"
gcloud run deploy "$SERVICE" \
  --source . \
  --region "$REGION" \
  --service-account "$RUN_SA" \
  --no-allow-unauthenticated \
  --max-instances 1 \
  --memory 256Mi \
  --set-env-vars "GOOGLE_CLOUD_PROJECT=$PROJECT_ID,GMAIL_PUBSUB_TOPIC=projects/$PROJECT_ID/topics/$TOPIC,RETENTION_DAYS=${RETENTION_DAYS:-90}" \
  --set-secrets "GOOGLE_OAUTH_CLIENT_JSON=google-oauth-client:latest,GOOGLE_OAUTH_TOKEN_JSON=google-oauth-token:latest" \
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

echo "== watch を今すぐ登録"
gcloud scheduler jobs run "$JOB" --location="$REGION"

cat <<EOF

デプロイしました。動作確認:
  1. 大学アドレスにテストメールを送る
  2. 1分ほど待ってログを確認する
     gcloud run services logs read $SERVICE --region $REGION --limit 20
  3. 「同期しました」のログで created が 1 以上になっていれば成功
EOF
