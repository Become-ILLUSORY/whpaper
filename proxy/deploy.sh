#!/usr/bin/env bash
# Deploy the whpaper mirror Worker to Cloudflare with nothing but curl.
#
#   export CF_API_TOKEN=...        # needs Workers Scripts:Edit + Zone DNS/Routes:Edit
#   export CF_ACCOUNT_ID=...       # dashboard → any page, URL /accounts/<id>/
#   export WH_HOST=wallpaper.example.com   # a hostname on a zone already in your account
#   export WHPAPER_TOKEN="$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 20)"
#   ./deploy.sh
#
# CF_ZONE_ID is resolved from WH_HOST automatically. Re-running is safe.
set -euo pipefail

API="${CF_API:-https://api.cloudflare.com/client/v4}"
TOKEN="${CF_API_TOKEN:?set CF_API_TOKEN}"
ACCOUNT="${CF_ACCOUNT_ID:?set CF_ACCOUNT_ID}"
HOST="${WH_HOST:?set WH_HOST, e.g. wallpaper.example.com}"
SCRIPT="${SCRIPT_NAME:-whpaper}"
SECRET="${WHPAPER_TOKEN:-}"
COMPAT_DATE="${COMPAT_DATE:-2026-09-01}"
DIR="$(cd "$(dirname "$0")" && pwd)"

if [ -z "$SECRET" ]; then
  echo "!! WHPAPER_TOKEN is empty — the mirror will be an OPEN proxy for wallhaven paths." >&2
fi

# Pull one field out of a JSON blob without needing jq.
json_field() { sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p' | head -1; }

check_ok() {
  printf '%s' "$1" | grep -qE '"success"[[:space:]]*:[[:space:]]*true' || {
    printf '%s\n' "$1" | head -c 600; echo; echo "!! Cloudflare API call failed" >&2; exit 1; }
}

ZONE_NAME="${HOST#*.}"
if [ -z "${CF_ZONE_ID:-}" ]; then
  Z="$(curl -sS --max-time 30 "$API/zones?name=$ZONE_NAME" -H "Authorization: Bearer $TOKEN")"
  check_ok "$Z"
  ZONE_ID="$(printf '%s' "$Z" | grep -o '"id":"[a-z0-9]*"' | head -1 | json_field id)"
else
  ZONE_ID="$CF_ZONE_ID"
fi
[ -n "$ZONE_ID" ] || { echo "zone $ZONE_NAME not found in this account; set CF_ZONE_ID" >&2; exit 1; }
echo "==> host=$HOST zone=$ZONE_ID account=$ACCOUNT script=$SCRIPT"

DNS="$(curl -sS --max-time 30 "$API/zones/$ZONE_ID/dns_records?name=$HOST" -H "Authorization: Bearer $TOKEN")"
if ! printf '%s' "$DNS" | grep -q '"id"'; then
  echo "==> creating proxied A record $HOST -> 192.0.2.1"
  R="$(curl -sS --max-time 30 -X POST "$API/zones/$ZONE_ID/dns_records" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d "{\"type\":\"A\",\"name\":\"$HOST\",\"content\":\"192.0.2.1\",\"proxied\":true,\"ttl\":1}")"
  check_ok "$R"
else
  echo "==> dns record already present"
fi

echo "==> uploading worker.js as '$SCRIPT'"
META="{\"main_module\":\"worker.js\",\"compatibility_date\":\"$COMPAT_DATE\",\"bindings\":[{\"type\":\"plain_text\",\"name\":\"WHPAPER_TOKEN\",\"text\":\"$SECRET\"}]}"
R="$(curl -sS --max-time 60 -X PUT "$API/accounts/$ACCOUNT/workers/scripts/$SCRIPT" \
  -H "Authorization: Bearer $TOKEN" \
  -F "metadata=$META;type=application/json" \
  -F "worker.js=@$DIR/worker.js;type=application/javascript+module")"
check_ok "$R"
echo "    deployed"

echo "==> route $HOST/* -> $SCRIPT"
# NOTE: the routes API wants the field named "script". Sending "script_name" is
# accepted on POST but stores a null binding, which shows up as error 522 at the edge.
ROUTES_RAW="$(curl -sS --max-time 30 "$API/zones/$ZONE_ID/workers/routes" -H "Authorization: Bearer $TOKEN")"
check_ok "$ROUTES_RAW"
compact="$(printf '%s' "$ROUTES_RAW" | tr -d ' \n\t')"
ROUTE_ID=""
OLDIFS="$IFS"; IFS='}'
for chunk in $compact; do
  case "$chunk" in
    *"\\"pattern\\":\\"$HOST/"*)
      ROUTE_ID="$(printf '%s' "$chunk" | grep -o '"id":"[a-z0-9]*"' | head -1 | cut -d'"' -f4)"
      ;;
  esac
done
IFS="$OLDIFS"

if [ -n "$ROUTE_ID" ]; then
  R="$(curl -sS --max-time 30 -X PUT "$API/zones/$ZONE_ID/workers/routes/$ROUTE_ID" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d "{\"pattern\":\"$HOST/*\",\"script\":\"$SCRIPT\"}")"
  check_ok "$R"; echo "    route updated"
else
  R="$(curl -sS --max-time 30 -X POST "$API/zones/$ZONE_ID/workers/routes" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d "{\"pattern\":\"$HOST/*\",\"script\":\"$SCRIPT\"}")"
  check_ok "$R"; echo "    route created"
fi

echo "==> health check"
curl -sS --max-time 20 "https://$HOST/healthz" | head -c 300; echo
echo "done.  base url: https://$HOST"
echo "     client:     whpaper config -init  # then set endpoints=[\"https://$HOST\", ...], token=\"$SECRET\""
