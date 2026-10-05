#!/usr/bin/env bash
# Creates, on our own instance (ssh hs) in the realm test-realm, the Keycloak client agw-agent through
# which the orchestrator exchanges the user token per chat (RFC 8693), and writes the client secret into
# .env. The secret never appears in the output. Can be run repeatedly: an existing client
# stays, only the secret is read again. Details: docs/keycloak-token-austausch.md in the thesis repo,
# section "Client agw-agent für den PoC".
#
#   dev/keycloak-agw-agent.sh            create (if needed), set standard flow and redirect URI
#                                        of the gateway (app.<base URL>/agent/), set .env
#   dev/keycloak-agw-agent.sh --delete   delete the client again
set -euo pipefail

HOST=${AGW_KC_SSH_HOST:-hs}
CONTAINER=${AGW_KC_CONTAINER:-agri_gaia-keycloak-1}
REALM=${AGW_KC_REALM:-test-realm}
CLIENT=agw-agent
BASE=${AGW_KC_BASE_URL:?set AGW_KC_BASE_URL (PROJECT_BASE_URL of the instance)}
ENV_FILE="$(cd "$(dirname "$0")/.." && pwd)/.env"

# Runs in the Keycloak container: admin login from KC_BOOTSTRAP_ADMIN_* (values never printed),
# configuration only for this run, deleted afterwards.
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
[ -n "$ID" ] && $K delete clients/$ID $C && echo "Client '"$CLIENT"' deleted" || echo "Client '"$CLIENT"' does not exist"'
  exit 0
fi

SECRET=$(remote "BASE=$BASE
"'
ID=$($K get clients $C -q clientId='"$CLIENT"' --fields id --format csv --noquotes | head -1)
if [ -z "$ID" ]; then
  ID=$($K create clients $C -s clientId='"$CLIENT"' -s "name=Agent orchestrator (PoC master thesis)" \
    -s "description=Authorization service of the PoC: logs the user in and exchanges their token per chat (RFC 8693)." \
    -s publicClient=false -s clientAuthenticatorType=client-secret -s standardFlowEnabled=false \
    -s implicitFlowEnabled=false -s directAccessGrantsEnabled=true -s serviceAccountsEnabled=false \
    -s "attributes.\"standard.token.exchange.enabled\"=true" -i)
  for a in backend minio; do
    $K create clients/$ID/protocol-mappers/models $C -s name=$a-audience -s protocol=openid-connect \
      -s protocolMapper=oidc-audience-mapper -s "config.\"included.client.audience\"=$a" \
      -s "config.\"access.token.claim\"=true" -s "config.\"id.token.claim\"=false" -s "config.\"userinfo.token.claim\"=false" >/dev/null
  done
  # minio_policy as for the client frontend: MinIO derives the rights for uploads from it.
  $K create clients/$ID/protocol-mappers/models $C -s name=minio_policy -s protocol=openid-connect \
    -s protocolMapper=oidc-usermodel-attribute-mapper -s "config.\"user.attribute\"=minio_policy" \
    -s "config.\"claim.name\"=minio_policy" -s "config.\"jsonType.label\"=String" \
    -s "config.\"access.token.claim\"=true" -s "config.\"id.token.claim\"=true" -s "config.\"userinfo.token.claim\"=true" >/dev/null
  echo "created" >&2
else
  echo "exists" >&2
fi
# Login through the platform (gateway as a platform service, since 2026-10-05): standard flow with PKCE and
# the redirect URI of the gateway. Also runs for an existing client; the password grant stays allowed for local
# development.
$K update clients/$ID $C -s standardFlowEnabled=true -s directAccessGrantsEnabled=true \
  -s "redirectUris=[\"https://app.$BASE/agent/oidc/callback\"]" -s "webOrigins=[\"https://app.$BASE\"]" \
  -s "attributes.\"pkce.code.challenge.method\"=S256" \
  -s "attributes.\"post.logout.redirect.uris\"=https://app.$BASE/agent/*" >/dev/null
echo "standard flow and redirect URI set" >&2
$K get clients/$ID/client-secret $C --fields value --format csv --noquotes
')

if [[ -z "$SECRET" ]]; then
  echo "no secret received" >&2
  exit 1
fi

# Set .env: replace existing lines, append missing ones. The secret only goes into the file.
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
echo "client $CLIENT ready; .env set (AGW_PLATFORM_CLIENT_ID, …_CLIENT_SECRET, …_TOKEN_EXCHANGE=true)."
echo "then: ./dev.sh start && docker restart agwpoc-orchestrator-1"
