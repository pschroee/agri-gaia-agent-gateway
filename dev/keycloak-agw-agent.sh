#!/usr/bin/env bash
# Legt auf der eigenen Instanz (ssh hs) im Realm test-realm den Keycloak-Client agw-agent an, über
# den der Orchestrator das Nutzertoken je Chat tauscht (RFC 8693), und trägt das Client-Secret in
# poc/.env ein. Das Secret erscheint nie in der Ausgabe. Mehrfach aufrufbar: Ein vorhandener Client
# bleibt, nur das Secret wird neu gelesen. Einzelheiten: docs/keycloak-token-austausch.md,
# Abschnitt „Client agw-agent für den PoC“.
#
#   poc/dev/keycloak-agw-agent.sh            anlegen (falls nötig) und .env setzen
#   poc/dev/keycloak-agw-agent.sh --delete   Client wieder löschen
set -euo pipefail

HOST=${AGW_KC_SSH_HOST:-hs}
CONTAINER=${AGW_KC_CONTAINER:-agri_gaia-keycloak-1}
REALM=${AGW_KC_REALM:-test-realm}
CLIENT=agw-agent
ENV_FILE="$(cd "$(dirname "$0")/.." && pwd)/.env"

# Läuft im Keycloak-Container: Admin-Anmeldung aus KC_BOOTSTRAP_ADMIN_* (Werte nie ausgegeben),
# Konfiguration nur für diesen Lauf, danach gelöscht.
remote() {
  ssh "$HOST" "sudo docker exec -i $CONTAINER sh -s" <<EOF
set -e
K=/opt/keycloak/bin/kcadm.sh
CFG=\$(mktemp)
trap 'rm -f \$CFG' EXIT
\$K config credentials --config \$CFG --server http://localhost:8080 --realm master \
  --user "\$KC_BOOTSTRAP_ADMIN_USERNAME" --password "\$KC_BOOTSTRAP_ADMIN_PASSWORD" >/dev/null 2>&1
C="--config \$CFG -r $REALM"
$1
EOF
}

if [[ "${1:-}" == "--delete" ]]; then
  remote 'ID=$($K get clients $C -q clientId='"$CLIENT"' --fields id --format csv --noquotes | head -1)
[ -n "$ID" ] && $K delete clients/$ID $C && echo "Client '"$CLIENT"' gelöscht" || echo "Client '"$CLIENT"' nicht vorhanden"'
  exit 0
fi

SECRET=$(remote '
ID=$($K get clients $C -q clientId='"$CLIENT"' --fields id --format csv --noquotes | head -1)
if [ -z "$ID" ]; then
  ID=$($K create clients $C -s clientId='"$CLIENT"' -s "name=Agent-Orchestrator (PoC Masterarbeit)" \
    -s "description=Autorisierungsdienst des PoC: meldet den Nutzer an und tauscht sein Token je Chat (RFC 8693)." \
    -s publicClient=false -s clientAuthenticatorType=client-secret -s standardFlowEnabled=false \
    -s implicitFlowEnabled=false -s directAccessGrantsEnabled=true -s serviceAccountsEnabled=false \
    -s "attributes.\"standard.token.exchange.enabled\"=true" -i)
  for a in backend minio; do
    $K create clients/$ID/protocol-mappers/models $C -s name=$a-audience -s protocol=openid-connect \
      -s protocolMapper=oidc-audience-mapper -s "config.\"included.client.audience\"=$a" \
      -s "config.\"access.token.claim\"=true" -s "config.\"id.token.claim\"=false" -s "config.\"userinfo.token.claim\"=false" >/dev/null
  done
  # minio_policy wie beim Client frontend: MinIO leitet daraus die Rechte für Uploads ab.
  $K create clients/$ID/protocol-mappers/models $C -s name=minio_policy -s protocol=openid-connect \
    -s protocolMapper=oidc-usermodel-attribute-mapper -s "config.\"user.attribute\"=minio_policy" \
    -s "config.\"claim.name\"=minio_policy" -s "config.\"jsonType.label\"=String" \
    -s "config.\"access.token.claim\"=true" -s "config.\"id.token.claim\"=true" -s "config.\"userinfo.token.claim\"=true" >/dev/null
  echo "angelegt" >&2
else
  echo "vorhanden" >&2
fi
$K get clients/$ID/client-secret $C --fields value --format csv --noquotes
')

if [[ -z "$SECRET" ]]; then
  echo "Kein Secret erhalten" >&2
  exit 1
fi

# .env setzen: vorhandene Zeilen ersetzen, fehlende anhängen. Das Secret geht nur in die Datei.
set_env() {
  local key=$1 val=$2
  if grep -q "^$key=" "$ENV_FILE" 2>/dev/null; then
    tmp=$(mktemp)
    awk -v k="$key" -v v="$val" 'BEGIN{FS=OFS="="} $1==k{print k"="v; next} {print}' "$ENV_FILE" >"$tmp" && mv "$tmp" "$ENV_FILE"
  else
    printf '%s=%s\n' "$key" "$val" >>"$ENV_FILE"
  fi
}
set_env AGW_PLATFORM_CLIENT_ID "$CLIENT"
set_env AGW_PLATFORM_CLIENT_SECRET "$SECRET"
set_env AGW_PLATFORM_TOKEN_EXCHANGE true
chmod 600 "$ENV_FILE"
echo "Client $CLIENT bereit; poc/.env gesetzt (AGW_PLATFORM_CLIENT_ID, …_CLIENT_SECRET, …_TOKEN_EXCHANGE=true)."
echo "Danach: cd poc && ./dev.sh start && docker restart agwpoc-orchestrator-1"
