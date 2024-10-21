#!/usr/bin/env bash

# Croco6 is a script that creates a session using a running instance of crocochrome and invokes k6 with the
# returned browser URL.

set -eo pipefail

# By default this assumes you're already running crocochrome in docker, e.g.:
# docker run --ti --rm -p 8080:8080 -p 5222:5222 localhost:5000/crocochrome
CROCOCHROME_URL=${CROCOCHROME_URL:-http://localhost:8080}
K6=${K6:-k6}

session=$(curl -sSL "$CROCOCHROME_URL"/sessions -d '{}')
id=$(echo "$session" | jq -r .id)
echo "Got session $id"

url=$(echo "$session" | jq -r .chromiumVersion.webSocketDebuggerUrl)
if [[ -z $url ]]; then
  echo "Crocochrome returned an empty url"
  exit 1
fi

echo "Browser URL: $url"

K6_BROWSER_WS_URL="$url" "$K6" "$@" || true

echo "Deleting session $id"
curl -sSL -X DELETE "$CROCOCHROME_URL/sessions/$id"
