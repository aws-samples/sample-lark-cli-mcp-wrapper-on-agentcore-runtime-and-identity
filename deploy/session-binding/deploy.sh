#!/usr/bin/env bash
# Copyright (c) 2026 panlm
# SPDX-License-Identifier: MIT
#
# Provisions the OAuth2 session-binding endpoint described in README.md, across
# two accounts: the Lambda and its public function URL in the operator's
# account, a narrowly scoped assumable role in the account holding AgentCore
# Identity. Idempotent — re-run after editing a policy.
#
# Set both accounts to the same profile to deploy single-account, in which case
# the AssumeRole hop is still used but stays within one account.

set -euo pipefail

OPERATOR_PROFILE="${OPERATOR_PROFILE:?set OPERATOR_PROFILE to the profile hosting the Lambda}"
AGENT_PROFILE="${AGENT_PROFILE:?set AGENT_PROFILE to the profile with AgentCore Identity}"
REGION="${AWS_REGION:-us-east-1}"

FUNCTION_NAME="${FUNCTION_NAME:-lark-session-binding}"
LAMBDA_ROLE="${LAMBDA_ROLE:-LarkSessionBindingLambdaRole}"
BINDING_ROLE="${BINDING_ROLE:-LarkCliMcpSessionBindingRole}"
WORKLOAD_NAME="${AGENTCORE_WORKLOAD_NAME:-feishu-mcp-workload}"
PROVIDER_NAME="${AGENTCORE_CREDENTIAL_PROVIDER_ID:-feishu-oauth-provider}"
USER_ID="${AGENTCORE_USER_ID:-bot}"

op() { aws --profile "$OPERATOR_PROFILE" --region "$REGION" "$@"; }
ag() { aws --profile "$AGENT_PROFILE" --region "$REGION" "$@"; }

OPERATOR_ACCOUNT=$(op sts get-caller-identity --query Account --output text)
AGENT_ACCOUNT=$(ag sts get-caller-identity --query Account --output text)
LAMBDA_ROLE_ARN="arn:aws:iam::${OPERATOR_ACCOUNT}:role/${LAMBDA_ROLE}"
BINDING_ROLE_ARN="arn:aws:iam::${AGENT_ACCOUNT}:role/${BINDING_ROLE}"

echo "operator account: ${OPERATOR_ACCOUNT}  (Lambda + function URL)"
echo "agent account:    ${AGENT_ACCOUNT}  (AgentCore Identity)"

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# Shared values. Reuse whatever the Lambda already has, so re-running does not
# invalidate the secret the runtime was configured with.
EXISTING_ENV=$(op lambda get-function-configuration --function-name "$FUNCTION_NAME" \
  --query 'Environment.Variables' --output json 2>/dev/null || echo '{}')
read_existing() { python3 -c "import json,sys; print(json.loads(sys.argv[1]).get(sys.argv[2],''))" "$EXISTING_ENV" "$1"; }

STATE_SECRET="${AGENTCORE_STATE_SECRET:-$(read_existing AGENTCORE_STATE_SECRET)}"
EXTERNAL_ID="${AGENTCORE_EXTERNAL_ID:-$(read_existing AGENTCORE_EXTERNAL_ID)}"
PATH_TOKEN="${CALLBACK_PATH_TOKEN:-$(read_existing CALLBACK_PATH_TOKEN)}"
[ -n "$STATE_SECRET" ] || STATE_SECRET=$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')
[ -n "$EXTERNAL_ID" ]  || EXTERNAL_ID=$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')
[ -n "$PATH_TOKEN" ]   || PATH_TOKEN=$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')

# ── Agent account: the role the Lambda assumes ────────────────────────────────

cat > "$WORK_DIR/binding-trust.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"AWS": "${LAMBDA_ROLE_ARN}"},
    "Action": "sts:AssumeRole",
    "Condition": {"StringEquals": {"sts:ExternalId": "${EXTERNAL_ID}"}}
  }]
}
JSON

# CompleteResourceTokenAuth internally reads the provider's client secret to
# redeem the authorization code, so GetSecretValue is required alongside it.
# The secret's name is assigned by AgentCore Identity; read it off the provider
# rather than guessing the suffix.
PROVIDER_SECRET_ARN=$(ag bedrock-agentcore-control get-oauth2-credential-provider \
  --name "$PROVIDER_NAME" --query 'clientSecretArn.secretArn' --output text)

cat > "$WORK_DIR/binding-perm.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "CompleteSessionBindingOnly",
      "Effect": "Allow",
      "Action": "bedrock-agentcore:CompleteResourceTokenAuth",
      "Resource": [
        "arn:aws:bedrock-agentcore:${REGION}:${AGENT_ACCOUNT}:workload-identity-directory/default",
        "arn:aws:bedrock-agentcore:${REGION}:${AGENT_ACCOUNT}:workload-identity-directory/default/workload-identity/${WORKLOAD_NAME}",
        "arn:aws:bedrock-agentcore:${REGION}:${AGENT_ACCOUNT}:token-vault/default",
        "arn:aws:bedrock-agentcore:${REGION}:${AGENT_ACCOUNT}:token-vault/default/oauth2credentialprovider/${PROVIDER_NAME}"
      ]
    },
    {
      "Sid": "ReadProviderClientSecret",
      "Effect": "Allow",
      "Action": "secretsmanager:GetSecretValue",
      "Resource": "${PROVIDER_SECRET_ARN}"
    }
  ]
}
JSON

if ag iam get-role --role-name "$BINDING_ROLE" >/dev/null 2>&1; then
  ag iam update-assume-role-policy --role-name "$BINDING_ROLE" \
    --policy-document "file://$WORK_DIR/binding-trust.json"
else
  ag iam create-role --role-name "$BINDING_ROLE" \
    --assume-role-policy-document "file://$WORK_DIR/binding-trust.json" \
    --description "Assumed by the session-binding Lambda in ${OPERATOR_ACCOUNT}" \
    --max-session-duration 3600 >/dev/null
fi
ag iam put-role-policy --role-name "$BINDING_ROLE" \
  --policy-name CompleteSessionBinding \
  --policy-document "file://$WORK_DIR/binding-perm.json"
echo "agent account: ${BINDING_ROLE} ready"

# ── Operator account: Lambda execution role ───────────────────────────────────

cat > "$WORK_DIR/lambda-trust.json" <<'JSON'
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "lambda.amazonaws.com"},
    "Action": "sts:AssumeRole"
  }]
}
JSON

# Logs are pinned to this function's own log group in this account, so a
# cross-account write is denied by IAM rather than merely unconfigured.
cat > "$WORK_DIR/lambda-perm.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "AssumeSessionBindingRole",
      "Effect": "Allow",
      "Action": "sts:AssumeRole",
      "Resource": "${BINDING_ROLE_ARN}"
    },
    {
      "Sid": "OwnAccountLogsOnly",
      "Effect": "Allow",
      "Action": ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"],
      "Resource": "arn:aws:logs:${REGION}:${OPERATOR_ACCOUNT}:log-group:/aws/lambda/${FUNCTION_NAME}:*"
    }
  ]
}
JSON

if ! op iam get-role --role-name "$LAMBDA_ROLE" >/dev/null 2>&1; then
  op iam create-role --role-name "$LAMBDA_ROLE" \
    --assume-role-policy-document "file://$WORK_DIR/lambda-trust.json" \
    --description "Execution role for the lark-cli OAuth session-binding Lambda" >/dev/null
  echo "waiting for IAM role propagation..."
  sleep 10
fi
op iam put-role-policy --role-name "$LAMBDA_ROLE" \
  --policy-name SessionBindingPermissions \
  --policy-document "file://$WORK_DIR/lambda-perm.json"
echo "operator account: ${LAMBDA_ROLE} ready"

# ── Operator account: the function itself ─────────────────────────────────────

(cd "$(dirname "$0")" && zip -q "$WORK_DIR/function.zip" handler.py)

ENV_VARS="{AGENTCORE_REGION=${REGION},AGENTCORE_USER_ID=${USER_ID},AGENTCORE_ASSUME_ROLE_ARN=${BINDING_ROLE_ARN},AGENTCORE_EXTERNAL_ID=${EXTERNAL_ID},AGENTCORE_STATE_SECRET=${STATE_SECRET},CALLBACK_PATH_TOKEN=${PATH_TOKEN}}"

if op lambda get-function --function-name "$FUNCTION_NAME" >/dev/null 2>&1; then
  op lambda update-function-code --function-name "$FUNCTION_NAME" \
    --zip-file "fileb://$WORK_DIR/function.zip" >/dev/null
  op lambda wait function-updated --function-name "$FUNCTION_NAME"
  op lambda update-function-configuration --function-name "$FUNCTION_NAME" \
    --environment "Variables=${ENV_VARS}" >/dev/null
  op lambda wait function-updated --function-name "$FUNCTION_NAME"
else
  op lambda create-function --function-name "$FUNCTION_NAME" \
    --runtime python3.13 --architectures arm64 \
    --role "$LAMBDA_ROLE_ARN" --handler handler.handler \
    --zip-file "fileb://$WORK_DIR/function.zip" \
    --timeout 15 --memory-size 256 \
    --description "OAuth2 session-binding endpoint for lark-cli MCP (AgentCore Identity hop 2)" \
    --environment "Variables=${ENV_VARS}" >/dev/null
  op lambda wait function-active-v2 --function-name "$FUNCTION_NAME"
fi
echo "operator account: ${FUNCTION_NAME} deployed"

# A browser redirect carries no SigV4, so the URL has to be AuthType NONE. Since
# October 2025 that requires BOTH lambda:InvokeFunctionUrl and
# lambda:InvokeFunction in the resource policy — with only the first, every
# request gets a 403 that looks like a propagation delay.
FUNCTION_URL=$(op lambda get-function-url-config --function-name "$FUNCTION_NAME" \
  --query FunctionUrl --output text 2>/dev/null || \
  op lambda create-function-url-config --function-name "$FUNCTION_NAME" \
    --auth-type NONE --query FunctionUrl --output text)

op lambda add-permission --function-name "$FUNCTION_NAME" \
  --statement-id FunctionURLAllowPublicAccess \
  --action lambda:InvokeFunctionUrl --principal '*' \
  --function-url-auth-type NONE >/dev/null 2>&1 || true
op lambda add-permission --function-name "$FUNCTION_NAME" \
  --statement-id FunctionURLInvokeAllowPublicAccess \
  --action lambda:InvokeFunction --principal '*' \
  --invoked-via-function-url >/dev/null 2>&1 || true

RETURN_URL="${FUNCTION_URL%/}/${PATH_TOKEN}"

# ── Agent account: allowlist the return URL ───────────────────────────────────
#
# The loopback entry is kept so local development, which relies on the sidecar's
# own callback listener, keeps working against the same workload identity.
ag bedrock-agentcore-control update-workload-identity --name "$WORKLOAD_NAME" \
  --allowed-resource-oauth2-return-urls "$RETURN_URL" "http://localhost:3000/callback" >/dev/null
echo "agent account: return URL registered on ${WORKLOAD_NAME}"

cat <<EOF

Done. Set these on the AgentCore runtime, then re-run the authorization:

  AGENTCORE_RETURN_URL=${RETURN_URL}
  AGENTCORE_STATE_SECRET=${STATE_SECRET}

Both are required. With only the return URL set, the sidecar sends no state and
the Lambda rejects every callback.

  aws bedrock-agentcore-control update-agent-runtime \\
    --agent-runtime-id <runtime-id> --region ${REGION} \\
    --environment-variables "AGENTCORE_RETURN_URL=${RETURN_URL},AGENTCORE_STATE_SECRET=${STATE_SECRET}" \\
    ... # plus the other required update-agent-runtime arguments

Verify: call lark_auth_start, complete the login, then call lark_exec directly —
lark_auth_complete should no longer be needed.
EOF
