#!/usr/bin/env bash
set -euo pipefail
if [ -n "${GOOGLE_SERVICES_BASE64:-}" ]; then
  echo "$GOOGLE_SERVICES_BASE64" | base64 -d > google-services.json
  echo "Wrote google-services.json from GOOGLE_SERVICES_BASE64"
fi
