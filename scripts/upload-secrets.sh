#!/usr/bin/env bash
# ローカルの .env と .secrets/ の値を Secret Manager に新しいバージョンとして登録する。
# 値は標準入力経由で渡すため、コマンド履歴やプロセス一覧には残らない。
#
#   PROJECT_ID=your-project ./scripts/upload-secrets.sh
set -euo pipefail

: "${PROJECT_ID:?PROJECT_ID を指定してください}"
cd "$(dirname "$0")/.."

# .env から1つの値だけを取り出す（シェルに読み込むと他の値まで環境に漏れるため source しない）。
env_value() { grep -E "^$1=" .env | head -1 | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//'; }

put() { # put <secret名> : 標準入力の内容を登録
  gcloud secrets versions add "$1" --project="$PROJECT_ID" --data-file=- >/dev/null
  echo "   $1 を登録しました"
}

[ -f .env ] || { echo ".env がありません"; exit 1; }

v=$(env_value ANTHROPIC_API_KEY);          [ -n "$v" ] && printf '%s' "$v" | put anthropic-api-key
v=$(env_value DISCORD_BOT_TOKEN);          [ -n "$v" ] && printf '%s' "$v" | put discord-bot-token
v=$(env_value LINE_CHANNEL_ACCESS_TOKEN);  [ -n "$v" ] && printf '%s' "$v" | put line-channel-access-token
[ -f .secrets/client_secret.json ] && put google-oauth-client < .secrets/client_secret.json
[ -f .secrets/token.json ]         && put google-oauth-token  < .secrets/token.json
echo "完了"
