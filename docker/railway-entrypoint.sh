#!/bin/sh
set -eu
umask 077

mkdir -p /run/grok2api /app/data /app/data/media
chown -R grok2api:grok2api /app/data /run/grok2api
chmod 750 /app/data

cat > /run/grok2api/config.yaml <<EOF
server:
  listen: "0.0.0.0:8000"

auth:
  secureCookies: true

secrets:
  jwtSecret: "${JWT_SECRET}"
  credentialEncryptionKey: "${CREDENTIAL_ENCRYPTION_KEY}"

bootstrapAdmin:
  username: "${ADMIN_USERNAME:-admin}"
  password: "${ADMIN_PASSWORD}"

frontend:
  staticPath: "./frontend/dist"

database:
  driver: sqlite
  sqlite:
    path: "./data/backend.db"

runtimeStore:
  driver: memory

media:
  driver: local
  local:
    path: "./data/media"
EOF

exec /usr/local/bin/grok2api-entrypoint "$@"
