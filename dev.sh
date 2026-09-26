#!/bin/bash
# Build and run the platform with config from platform/.env
set -euo pipefail
cd "$(dirname "$0")"
set -a; source .env; set +a
go build -o platform .
for pid in $(lsof -nP -tiTCP:8080 -sTCP:LISTEN 2>/dev/null); do kill "$pid" 2>/dev/null || true; done
sleep 1
exec ./platform
