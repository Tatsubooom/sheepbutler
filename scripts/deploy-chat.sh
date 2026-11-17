#!/usr/bin/env bash
# Discord のスラッシュコマンドとボタンを受け取る公開サービス（sheepbutler-chat）をデプロイし、
# Discord 側にコマンドと送り先 URL を登録する。scripts/deploy.sh の最後から呼ばれる。単独でも実行できる。
#
#   PROJECT_ID=your-project ./scripts/deploy-chat.sh
#
# 認証なしで公開するが、すべての要求で Discord の署名を検証し、Bot の持ち主以外の操作は拒否する。
# 前提: .env に DISCORD_BOT_TOKEN と DISCORD_CHANNEL_ID、手元に Go があること。
set -euo pipefail

: "${PROJECT_ID:?PROJECT_ID を指定してください}"
REGION="${REGION:-asia-northeast1}"
SERVICE="${SERVICE:-sheepbutler}"
CHAT_SERVICE="${CHAT_SERVICE:-sheepbutler-chat}"
CHAT_NAME="sheepbutler-chat"
CHAT_SA="$CHAT_NAME@$PROJECT_ID.iam.gserviceaccount.com"
CHAT_TOPIC="${CHAT_TOPIC:-chat-jobs}"

cd "$(dirname "$0")/.."
env_value() { local v="${!1:-}"; [ -n "$v" ] && { echo "$v"; return; }; grep -E "^$1=" .env 2>/dev/null | head -1 | cut -d= -f2- || true; }
has_secret() { gcloud secrets versions describe latest --secret="$1" >/dev/null 2>&1; }

if [ "$(env_value NOTIFY_CHANNEL)" = "line" ]; then
  echo "== 通知先が LINE のため、Discord のチャットは設定しない"
  exit 0
fi

echo "== Discord のアプリ情報を取得"
INFO=$(go run ./cmd/discordsetup info | tr -d '\r')
get() { echo "$INFO" | grep -E "^$1=" | cut -d= -f2-; }
PUBLIC_KEY=$(get DISCORD_PUBLIC_KEY)
OWNER_ID=$(get DISCORD_OWNER_ID)
[ -n "$PUBLIC_KEY" ] && [ -n "$OWNER_ID" ] || { echo "Discord のアプリ情報を取得できません"; exit 1; }

echo "== チャット用サービスアカウント"
if ! gcloud iam service-accounts describe "$CHAT_SA" >/dev/null 2>&1; then
  gcloud iam service-accounts create "$CHAT_NAME" --display-name="SheepButler chat (Discord)"
fi
# 選考状況・一時停止の読み書き（Firestore）と、予定の読み取りに必要な秘密情報だけを与える。
gcloud projects add-iam-policy-binding "$PROJECT_ID" --member="serviceAccount:$CHAT_SA" \
  --role=roles/datastore.user --condition=None >/dev/null
# /ask の質問を本体に渡すトピックへの発行だけを許す（トピックは scripts/deploy.sh が作る）。
gcloud pubsub topics add-iam-policy-binding "$CHAT_TOPIC" --member="serviceAccount:$CHAT_SA" --role=roles/pubsub.publisher >/dev/null
SECRETS_ARG=""
for pair in "GOOGLE_OAUTH_CLIENT_JSON=google-oauth-client" "GOOGLE_OAUTH_TOKEN_JSON=google-oauth-token" "/config/config.yaml=app-config"; do
  secret="${pair#*=}"
  if has_secret "$secret"; then
    gcloud secrets add-iam-policy-binding "$secret" --member="serviceAccount:$CHAT_SA" \
      --role=roles/secretmanager.secretAccessor >/dev/null
    SECRETS_ARG="${SECRETS_ARG:+$SECRETS_ARG,}$pair:latest"
  fi
done
ENV_ARG="GOOGLE_CLOUD_PROJECT=$PROJECT_ID|DISCORD_PUBLIC_KEY=$PUBLIC_KEY|DISCORD_OWNER_ID=$OWNER_ID|GOOGLE_CALENDAR_ID=$(env_value GOOGLE_CALENDAR_ID)|CHAT_JOBS_TOPIC=projects/$PROJECT_ID/topics/$CHAT_TOPIC"
case "$SECRETS_ARG" in *app-config*) ENV_ARG="$ENV_ARG|CONFIG_PATH=/config/config.yaml" ;; esac

echo "== チャットを Cloud Run にデプロイ（本体と同じイメージを使う、公開）"
IMAGE=$(gcloud run services describe "$SERVICE" --region "$REGION" --format='value(spec.template.spec.containers[0].image)')
# Windows の Git Bash がパスを書き換えないよう、これらの引数は変換しない。
MSYS2_ARG_CONV_EXCL="--command=;--set-env-vars=;--set-secrets=" gcloud run deploy "$CHAT_SERVICE" \
  --image "$IMAGE" \
  --command=/chat \
  --region "$REGION" \
  --service-account "$CHAT_SA" \
  --allow-unauthenticated \
  --max-instances 1 \
  --memory 128Mi \
  --set-env-vars="^|^$ENV_ARG" \
  ${SECRETS_ARG:+--set-secrets="$SECRETS_ARG"} \
  --quiet >/dev/null
CHAT_URL=$(gcloud run services describe "$CHAT_SERVICE" --region "$REGION" --format='value(status.url)')
echo "   URL: $CHAT_URL"

echo "== Discord にコマンドと送り先 URL を登録"
go run ./cmd/discordsetup register -url "$CHAT_URL/interactions"

cat <<EOF

チャットを設定しました。Discord で /status /deadlines /today /pause /resume /ask が使えます。
/ask で頼んだ変更（選考状況・志望度・設定）は「反映する」ボタンを押したときだけ反映されます。
選考状況が変わったメールの通知には「確定」「取り消す」のボタンが付きます。
EOF
