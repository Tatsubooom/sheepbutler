#!/usr/bin/env bash
# Google Cloud 側の M0 準備をまとめて行う。何度実行しても壊れないように、既存のリソースはスキップする。
#
#   PROJECT_ID=your-project ./scripts/gcp-setup.sh
#
# 事前に gcloud をインストールし、`gcloud auth login` を済ませておくこと。
# OAuth 同意画面とクライアント ID の作成は gcloud では行えないため、docs/setup/m0.md の手順でコンソールから行う。
set -euo pipefail

: "${PROJECT_ID:?PROJECT_ID を指定してください（例: PROJECT_ID=sheepbutler-12345）}"
REGION="${REGION:-asia-northeast1}"
TOPIC="${TOPIC:-gmail-push}"
RUN_SA="${RUN_SA:-sheepbutler-run}"
SECRETS=(anthropic-api-key discord-bot-token line-channel-access-token google-oauth-client google-oauth-token app-config)

gcloud config set project "$PROJECT_ID" >/dev/null

echo "== API を有効化"
gcloud services enable \
  gmail.googleapis.com calendar-json.googleapis.com pubsub.googleapis.com \
  firestore.googleapis.com run.googleapis.com cloudscheduler.googleapis.com \
  secretmanager.googleapis.com artifactregistry.googleapis.com cloudbuild.googleapis.com \
  cloudbilling.googleapis.com billingbudgets.googleapis.com

echo "== Firestore（ネイティブモード, $REGION）"
if ! gcloud firestore databases describe --database='(default)' >/dev/null 2>&1; then
  gcloud firestore databases create --location="$REGION" --type=firestore-native
else
  echo "   既に存在するのでスキップ"
fi

# messages・audit・notifications の expiresAt を過ぎたドキュメントを自動削除する（保存期間の上限）。
echo "== Firestore の TTL ポリシー（messages, audit, notifications の expiresAt）"
gcloud firestore fields ttls update expiresAt --collection-group=messages --enable-ttl --async >/dev/null
gcloud firestore fields ttls update expiresAt --collection-group=audit --enable-ttl --async >/dev/null
gcloud firestore fields ttls update expiresAt --collection-group=notifications --enable-ttl --async >/dev/null

echo "== Pub/Sub トピック $TOPIC（Gmail の新着通知を受け取る）"
if ! gcloud pubsub topics describe "$TOPIC" >/dev/null 2>&1; then
  gcloud pubsub topics create "$TOPIC"
else
  echo "   既に存在するのでスキップ"
fi
# Gmail のシステムアカウントにだけ、このトピックへの publish を許可する。
gcloud pubsub topics add-iam-policy-binding "$TOPIC" \
  --member=serviceAccount:gmail-api-push@system.gserviceaccount.com \
  --role=roles/pubsub.publisher >/dev/null

echo "== Cloud Run 用サービスアカウント $RUN_SA"
SA_EMAIL="$RUN_SA@$PROJECT_ID.iam.gserviceaccount.com"
if ! gcloud iam service-accounts describe "$SA_EMAIL" >/dev/null 2>&1; then
  gcloud iam service-accounts create "$RUN_SA" --display-name="SheepButler (Cloud Run)"
fi
# 必要最小限の権限のみ付与する（Firestore の読み書き）。シークレットは個別に付与する。
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:$SA_EMAIL" --role=roles/datastore.user --condition=None >/dev/null

echo "== Secret Manager（空の箱を作り、Cloud Run のサービスアカウントに読み取りを許可）"
for s in "${SECRETS[@]}"; do
  if ! gcloud secrets describe "$s" >/dev/null 2>&1; then
    gcloud secrets create "$s" --replication-policy=automatic
  fi
  gcloud secrets add-iam-policy-binding "$s" \
    --member="serviceAccount:$SA_EMAIL" --role=roles/secretmanager.secretAccessor >/dev/null
done

cat <<EOF

完了しました。次の手順:
  1. docs/setup/m0.md に従って OAuth クライアントを作り、go run ./cmd/authorize を実行
  2. go run ./cmd/m0check で接続を確認
  3. 確認できたら ./scripts/upload-secrets.sh で .env と .secrets の値を Secret Manager に登録
  4. ./scripts/deploy.sh でデプロイ（予算の監視と自動停止も設定される）
EOF
