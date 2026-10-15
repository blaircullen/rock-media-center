#!/usr/bin/env bash
# Host-side watchdog for Rock Media Center's music engine (U16, KTD13, R30).
# Runs from cron every 10 minutes, outside the app. Reads the engine heartbeat and sends ONE
# Telegram message when it is unreachable or stale, and ONE when it recovers.
#
# Usage:  rmc-watchdog.sh [env-file]        (default env file: ~/.config/rmc-watchdog.env)
#
# The env file must be mode 600 and set TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID. It may also
# set RMC_URL, RMC_STATE_FILE, RMC_STALE_SECS, RMC_CURL_MAX_TIME, TELEGRAM_API (all optional;
# TELEGRAM_API exists so tests can point at a local stub).
#
# ASSUMPTION for U13: GET $RMC_URL/api/v1/musicengine/heartbeat answers without a session
# with JSON {"last_tick": <unix seconds>}. internal/musicengine/heartbeat.go currently names
# the field "last_tick_at"; this script accepts either name. 0 or missing means "no tick yet",
# which counts as stale.
#
# Exit codes:
#   0  healthy (fresh heartbeat)
#   1  unhealthy: unreachable or stale (alert sent now, or already sent for this outage)
#   2  usage or config error (env file missing/unsafe/incomplete, missing tool)
#   3  unhealthy and the Telegram alert could not be delivered (will retry next run)
set -u
umask 077

ENV_FILE="${1:-$HOME/.config/rmc-watchdog.env}"

die() { echo "rmc-watchdog: $*" >&2; exit 2; }

for tool in curl jq stat date; do
  command -v "$tool" >/dev/null 2>&1 || die "missing tool: $tool"
done

[ -f "$ENV_FILE" ] || die "env file not found: $ENV_FILE"
# Mode must be 600 (owner read/write only): it holds the bot token.
mode="$(stat -c '%a' "$ENV_FILE" 2>/dev/null || stat -f '%Lp' "$ENV_FILE" 2>/dev/null)" || die "cannot stat $ENV_FILE"
[ "$mode" = "600" ] || die "env file $ENV_FILE has mode $mode, must be 600"

# shellcheck disable=SC1090
. "$ENV_FILE"
[ -n "${TELEGRAM_BOT_TOKEN:-}" ] || die "TELEGRAM_BOT_TOKEN not set in $ENV_FILE"
[ -n "${TELEGRAM_CHAT_ID:-}" ] || die "TELEGRAM_CHAT_ID not set in $ENV_FILE"

RMC_URL="${RMC_URL:-http://10.0.0.13:8787}"
STATE_FILE="${RMC_STATE_FILE:-$HOME/.local/state/rmc-watchdog.state}"
STALE_SECS="${RMC_STALE_SECS:-1200}"          # 20 minutes
CURL_MAX_TIME="${RMC_CURL_MAX_TIME:-15}"       # hard total deadline per request
TELEGRAM_API="${TELEGRAM_API:-https://api.telegram.org}"
HEARTBEAT_URL="${RMC_URL%/}/api/v1/musicengine/heartbeat"

mkdir -p "$(dirname "$STATE_FILE")" || die "cannot create state dir for $STATE_FILE"

# State file holds one word: "alerted" while an outage has been announced, else absent/"ok".
state="ok"
[ -f "$STATE_FILE" ] && state="$(head -c 16 "$STATE_FILE" | tr -d '[:space:]')"

send_telegram() {
  # $1 = message text. The token is passed via a curl config on stdin so it never appears in
  # the process list. Returns 0 only when Telegram answers ok:true.
  local text="$1" resp
  resp="$(printf 'url = "%s/bot%s/sendMessage"\n' "$TELEGRAM_API" "$TELEGRAM_BOT_TOKEN" |
    curl -sS --max-time 20 -K - \
      --data-urlencode "chat_id=${TELEGRAM_CHAT_ID}" \
      --data-urlencode "text=${text}" 2>&1)"
  if [ "$(printf '%s' "$resp" | jq -r '.ok // false' 2>/dev/null)" = "true" ]; then
    return 0
  fi
  # Keep the reason: Telegram's JSON error (no token in it) or curl's message.
  echo "rmc-watchdog: telegram send failed: $(printf '%s' "$resp" | head -c 300)" >&2
  return 1
}

# --- probe -----------------------------------------------------------------------------
problem=""
body="$(curl -sS --max-time "$CURL_MAX_TIME" -w '\n%{http_code}' "$HEARTBEAT_URL" 2>&1)"
curl_rc=$?
if [ "$curl_rc" -ne 0 ]; then
  problem="heartbeat unreachable (curl exit $curl_rc: $(printf '%s' "$body" | head -c 160))"
else
  code="${body##*$'\n'}"
  json="${body%$'\n'*}"
  if [ "$code" != "200" ]; then
    problem="heartbeat returned HTTP $code: $(printf '%s' "$json" | head -c 160)"
  else
    last="$(printf '%s' "$json" | jq -r '(.last_tick // .last_tick_at // 0) | floor' 2>/dev/null)"
    case "$last" in
      ''|*[!0-9]*) problem="heartbeat body not understood: $(printf '%s' "$json" | head -c 160)" ;;
      *)
        now="$(date +%s)"
        if [ "$last" -le 0 ]; then
          problem="engine has not completed a tick since it started"
        else
          age=$((now - last))
          [ "$age" -gt "$STALE_SECS" ] && problem="last engine tick was $((age / 60)) minutes ago (limit $((STALE_SECS / 60)))"
        fi
        ;;
    esac
  fi
fi

# --- act (alert once per outage, once on recovery) -------------------------------------
if [ -n "$problem" ]; then
  echo "rmc-watchdog: UNHEALTHY: $problem"
  if [ "$state" = "alerted" ]; then
    exit 1
  fi
  if send_telegram "Rock Media Center music engine is not reporting: $problem. Checked $(date '+%Y-%m-%d %H:%M %Z') from Beast."; then
    printf 'alerted\n' >"$STATE_FILE" || die "alert sent but cannot write $STATE_FILE"
    exit 1
  fi
  exit 3
fi

echo "rmc-watchdog: ok (last tick $(( $(date +%s) - last ))s ago)"
if [ "$state" = "alerted" ]; then
  if send_telegram "Rock Media Center music engine is reporting again ($(date '+%Y-%m-%d %H:%M %Z'))."; then
    printf 'ok\n' >"$STATE_FILE" || die "recovery sent but cannot write $STATE_FILE"
  else
    exit 3   # keep state "alerted" so the recovery message is retried next run
  fi
fi
exit 0
