#!/bin/sh
# Render SearXNG settings with outgoing.proxies when PROXY_SEARXNG=on, then
# hand over to the stock SearXNG entrypoint. Credentials come from the
# environment and are only written to /tmp inside the container.
set -eu

if [ "${PROXY_SEARXNG:-off}" = "on" ] && [ -n "${PROXY_ENDPOINT:-}" ]; then
  # curl_cffi (used by SearXNG) only understands http:// proxy prefixes.
  SEARX_PROXY_URL="$PROXY_ENDPOINT"
  if [ "${PROXY_SEARXNG_SCHEME:-http}" = "http" ]; then
    SEARX_PROXY_URL=$(printf '%s' "$PROXY_ENDPOINT" | sed 's#^https://#http://#')
  fi
  python3 /etc/searxng/render_settings.py \
    /etc/searxng/settings.yml /tmp/searxng-settings.yml "$SEARX_PROXY_URL"
  export SEARXNG_SETTINGS_PATH=/tmp/searxng-settings.yml
fi

exec /usr/local/searxng/entrypoint.sh "$@"
