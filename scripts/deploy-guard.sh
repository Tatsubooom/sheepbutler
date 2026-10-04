#!/usr/bin/env bash
# 予算の監視と自動停止（ガード）を設定する。scripts/deploy.sh の最後から呼ばれる。単独でも実行できる。
#
#   PROJECT_ID=your-project ./scripts/deploy-guard.sh
#
# 予算（既定: 月 100 円）に対して
#   50% 以上 → SheepButler を一時停止（データは残る。deploy.sh の再実行で再開）
#   100% 以上 → プロジェクトの課金を外して全サービスを停止
# 費用データは数時間〜1日遅れて届くため、止まるまでにわずかに超えることがある。
set -euo pipefail

: "${PROJECT_ID:?PROJECT_ID を指定してください}"
REGION="${REGION:-asia-northeast1}"
SERVICE="${SERVICE:-sheepbutler}"
GUARD_SERVICE="${GUARD_SERVICE:-sheepbutler-guard}"
GUARD_NAME="sheepbutler-guard"
GUARD_SA="$GUARD_NAME@$PROJECT_ID.iam.gserviceaccount.com"
INVOKER_SA="sheepbutler-invoker@$PROJECT_ID.iam.gserviceaccount.com"
BILLING_TOPIC="${BILLING_TOPIC:-billing-alerts}"
BILLING_SUB="${BILLING_SUB:-billing-alerts-sub}"
BUDGET_NAME="${BUDGET_NAME:-SheepButler guard}"
# ガードが止める対象（deploy.sh で作るもの。複数なら ; 区切り）
STOP_JOBS="${STOP_JOBS:-renew-gmail-watch;sheepbutler-tick}"
STOP_SUBS="${STOP_SUBS:-gmail-push-sub}"

cd "$(dirname "$0")/.."
env_value() { grep -E "^$1=" .env 2>/dev/null | head -1 | cut -d= -f2- || true; }

echo "== 請求先アカウントと通貨"
BA=$(gcloud billing projects describe "$PROJECT_ID" --format='value(billingAccountName)')
BA="${BA#billingAccounts/}"
[ -n "$BA" ] || { echo "プロジェクトに請求先アカウントがリンクされていません"; exit 1; }
CUR=$(gcloud billing accounts describe "$BA" --format='value(currencyCode)')
case "$CUR" in
  JPY) DEFAULT_AMOUNT=100 ;;
  USD) DEFAULT_AMOUNT=1 ;;
  *)   DEFAULT_AMOUNT="" ;;
esac
AMOUNT="${BUDGET_AMOUNT:-$DEFAULT_AMOUNT}"
[ -n "$AMOUNT" ] || { echo "通貨 $CUR の既定予算がないので BUDGET_AMOUNT を指定してください"; exit 1; }
echo "   予算: 月 $AMOUNT $CUR（クレジットは差し引かずに計算）"

echo "== ガード用サービスアカウント"
if ! gcloud iam service-accounts describe "$GUARD_SA" >/dev/null 2>&1; then
  gcloud iam service-accounts create "$GUARD_NAME" --display-name="SheepButler guard (budget stop)"
fi
# 止めるために必要な権限だけを与える。
#   ジョブの一時停止 / プロジェクトの課金の解除 / Gmail 通知サブスクリプションの push 解除
gcloud projects add-iam-policy-binding "$PROJECT_ID" --member="serviceAccount:$GUARD_SA" \
  --role=roles/cloudscheduler.admin --condition=None >/dev/null
gcloud projects add-iam-policy-binding "$PROJECT_ID" --member="serviceAccount:$GUARD_SA" \
  --role=roles/billing.projectManager --condition=None >/dev/null
for s in ${STOP_SUBS//;/ }; do
  gcloud pubsub subscriptions add-iam-policy-binding "$s" --member="serviceAccount:$GUARD_SA" \
    --role=roles/pubsub.editor >/dev/null
done

echo "== 予算通知用の Pub/Sub トピック"
if ! gcloud pubsub topics describe "$BILLING_TOPIC" >/dev/null 2>&1; then
  gcloud pubsub topics create "$BILLING_TOPIC" >/dev/null
fi
gcloud pubsub topics add-iam-policy-binding "$BILLING_TOPIC" \
  --member=serviceAccount:billing-budget-alert@system.gserviceaccount.com \
  --role=roles/pubsub.publisher >/dev/null

echo "== ガードを Cloud Run にデプロイ（本体と同じイメージを使う）"
IMAGE=$(gcloud run services describe "$SERVICE" --region "$REGION" --format='value(spec.template.spec.containers[0].image)')
# 区切りは | （値の中の一覧は ; 区切り）。gcloud の "^|^" 記法で区切り文字を変える。
ENV_VARS="GOOGLE_CLOUD_PROJECT=$PROJECT_ID|GUARD_REGION=$REGION|GUARD_JOBS=${STOP_JOBS}|GUARD_SUBSCRIPTIONS=${STOP_SUBS}"
SECRET_ARGS=()
DISCORD_CHANNEL_ID="${DISCORD_CHANNEL_ID:-$(env_value DISCORD_CHANNEL_ID)}"
if [ -n "$DISCORD_CHANNEL_ID" ] && gcloud secrets versions describe latest --secret=discord-bot-token >/dev/null 2>&1; then
  ENV_VARS="$ENV_VARS|DISCORD_CHANNEL_ID=$DISCORD_CHANNEL_ID"
  SECRET_ARGS=(--set-secrets "DISCORD_BOT_TOKEN=discord-bot-token:latest")
  gcloud secrets add-iam-policy-binding discord-bot-token --member="serviceAccount:$GUARD_SA" \
    --role=roles/secretmanager.secretAccessor >/dev/null
else
  echo "   Discord が未設定のため、停止の連絡はしない"
fi
# Windows の Git Bash は "/guard" を "C:/Program Files/Git/guard" に書き換えるため、この引数だけ変換を止める。
MSYS2_ARG_CONV_EXCL="--command=" gcloud run deploy "$GUARD_SERVICE" \
  --image "$IMAGE" \
  --command=/guard \
  --region "$REGION" \
  --service-account "$GUARD_SA" \
  --no-allow-unauthenticated \
  --max-instances 1 \
  --memory 128Mi \
  --set-env-vars "^|^$ENV_VARS" \
  "${SECRET_ARGS[@]}" \
  --quiet >/dev/null
GUARD_URL=$(gcloud run services describe "$GUARD_SERVICE" --region "$REGION" --format='value(status.url)')
gcloud run services add-iam-policy-binding "$GUARD_SERVICE" --region "$REGION" \
  --member="serviceAccount:$INVOKER_SA" --role=roles/run.invoker >/dev/null

echo "== 予算通知の push サブスクリプション"
PUSH_ARGS=(
  --push-endpoint="$GUARD_URL/pubsub/billing"
  --push-auth-service-account="$INVOKER_SA"
  --push-auth-token-audience="$GUARD_URL"
  --ack-deadline=60
)
if gcloud pubsub subscriptions describe "$BILLING_SUB" >/dev/null 2>&1; then
  gcloud pubsub subscriptions update "$BILLING_SUB" "${PUSH_ARGS[@]}" >/dev/null
else
  gcloud pubsub subscriptions create "$BILLING_SUB" --topic="$BILLING_TOPIC" "${PUSH_ARGS[@]}" >/dev/null
fi

echo "== 予算"
PROJECT_NUMBER=$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')
BUDGET_ID=$(gcloud billing budgets list --billing-account="$BA" \
  --filter="displayName=\"$BUDGET_NAME\"" --format='value(name)' | head -1)
if [ -n "$BUDGET_ID" ]; then
  gcloud billing budgets update "$BUDGET_ID" --billing-account="$BA" \
    --budget-amount="${AMOUNT}${CUR}" \
    --notifications-rule-pubsub-topic="projects/$PROJECT_ID/topics/$BILLING_TOPIC" >/dev/null
else
  gcloud billing budgets create --billing-account="$BA" \
    --display-name="$BUDGET_NAME" \
    --budget-amount="${AMOUNT}${CUR}" \
    --calendar-period=month \
    --filter-projects="projects/$PROJECT_NUMBER" \
    --credit-types-treatment=exclude-all-credits \
    --threshold-rule=percent=0.5 \
    --threshold-rule=percent=0.9 \
    --threshold-rule=percent=1.0 \
    --notifications-rule-pubsub-topic="projects/$PROJECT_ID/topics/$BILLING_TOPIC" >/dev/null
fi

cat <<EOF

ガードを設定しました（予算: 月 $AMOUNT $CUR、50% で一時停止、100% で課金を解除）。
一時停止が動くか試すには、偽の予算通知を送る（SheepButler が止まり、Discord に連絡が来る）:
  gcloud pubsub topics publish $BILLING_TOPIC --message='{"costAmount":60,"budgetAmount":100,"currencyCode":"$CUR"}'
確認後は ./scripts/deploy.sh を再実行して再開する。
EOF
