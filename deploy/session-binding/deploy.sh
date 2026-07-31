#!/usr/bin/env bash
# Copyright (c) 2026 panlm
# SPDX-License-Identifier: MIT
#
# Provisions the OAuth2 session-binding endpoint described in README.md:
# a Lambda behind CloudFront with origin access control, plus WAF, all in the
# account that holds AgentCore Identity. Idempotent — re-run after editing a
# policy or the handler.
#
# The function URL is left AuthType AWS_IAM with a resource policy naming only
# cloudfront.amazonaws.com for this distribution, so the only reachable entry
# point is the CloudFront domain.

set -euo pipefail

REGION="${AWS_REGION:-us-east-1}"
FUNCTION_NAME="${FUNCTION_NAME:-lark-session-binding}"
LAMBDA_ROLE="${LAMBDA_ROLE:-LarkSessionBindingLambdaRole}"
OAC_NAME="${OAC_NAME:-lark-session-binding-oac}"
WAF_NAME="${WAF_NAME:-lark-session-binding-waf}"
WORKLOAD_NAME="${AGENTCORE_WORKLOAD_NAME:-feishu-mcp-workload}"
PROVIDER_NAME="${AGENTCORE_CREDENTIAL_PROVIDER_ID:-feishu-oauth-provider}"
USER_ID="${AGENTCORE_USER_ID:-bot}"

# CloudFront is global; its API and CLOUDFRONT-scoped WAF only answer in us-east-1.
aws_() { aws --region "$REGION" "$@"; }
cf_() { aws --region us-east-1 "$@"; }

ACCOUNT=$(aws_ sts get-caller-identity --query Account --output text)
LAMBDA_ROLE_ARN="arn:aws:iam::${ACCOUNT}:role/${LAMBDA_ROLE}"
echo "account: ${ACCOUNT}  region: ${REGION}"

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# Reuse whatever the function already has, so re-running does not invalidate the
# secret the runtime was configured with or move the callback path.
EXISTING_ENV=$(aws_ lambda get-function-configuration --function-name "$FUNCTION_NAME" \
  --query 'Environment.Variables' --output json 2>/dev/null || echo '{}')
read_existing() { python3 -c "import json,sys; print(json.loads(sys.argv[1]).get(sys.argv[2],''))" "$EXISTING_ENV" "$1"; }

STATE_SECRET="${AGENTCORE_STATE_SECRET:-$(read_existing AGENTCORE_STATE_SECRET)}"
PATH_TOKEN="${CALLBACK_PATH_TOKEN:-$(read_existing CALLBACK_PATH_TOKEN)}"
[ -n "$STATE_SECRET" ] || STATE_SECRET=$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')
[ -n "$PATH_TOKEN" ]   || PATH_TOKEN=$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')

# ── Execution role ────────────────────────────────────────────────────────────

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

# CompleteResourceTokenAuth internally reads the provider's client secret to
# redeem the authorization code, so GetSecretValue is required alongside it. The
# secret's name is assigned by AgentCore Identity — read it off the provider
# rather than guessing the suffix.
PROVIDER_SECRET_ARN=$(aws_ bedrock-agentcore-control get-oauth2-credential-provider \
  --name "$PROVIDER_NAME" --query 'clientSecretArn.secretArn' --output text)

cat > "$WORK_DIR/lambda-perm.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "CompleteSessionBindingOnly",
      "Effect": "Allow",
      "Action": "bedrock-agentcore:CompleteResourceTokenAuth",
      "Resource": [
        "arn:aws:bedrock-agentcore:${REGION}:${ACCOUNT}:workload-identity-directory/default",
        "arn:aws:bedrock-agentcore:${REGION}:${ACCOUNT}:workload-identity-directory/default/workload-identity/${WORKLOAD_NAME}",
        "arn:aws:bedrock-agentcore:${REGION}:${ACCOUNT}:token-vault/default",
        "arn:aws:bedrock-agentcore:${REGION}:${ACCOUNT}:token-vault/default/oauth2credentialprovider/${PROVIDER_NAME}"
      ]
    },
    {
      "Sid": "ReadProviderClientSecret",
      "Effect": "Allow",
      "Action": "secretsmanager:GetSecretValue",
      "Resource": "${PROVIDER_SECRET_ARN}"
    },
    {
      "Sid": "OwnLogGroupOnly",
      "Effect": "Allow",
      "Action": ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"],
      "Resource": "arn:aws:logs:${REGION}:${ACCOUNT}:log-group:/aws/lambda/${FUNCTION_NAME}:*"
    }
  ]
}
JSON

if ! aws_ iam get-role --role-name "$LAMBDA_ROLE" >/dev/null 2>&1; then
  aws_ iam create-role --role-name "$LAMBDA_ROLE" \
    --assume-role-policy-document "file://$WORK_DIR/lambda-trust.json" \
    --description "Execution role for the lark-cli OAuth session-binding Lambda" >/dev/null
  echo "waiting for IAM role propagation..."
  sleep 10
fi
aws_ iam put-role-policy --role-name "$LAMBDA_ROLE" \
  --policy-name SessionBindingPermissions \
  --policy-document "file://$WORK_DIR/lambda-perm.json"
echo "role ${LAMBDA_ROLE} ready"

# ── Function ──────────────────────────────────────────────────────────────────

(cd "$(dirname "$0")" && zip -q "$WORK_DIR/function.zip" handler.py)

ENV_VARS="{AGENTCORE_REGION=${REGION},AGENTCORE_USER_ID=${USER_ID},AGENTCORE_STATE_SECRET=${STATE_SECRET},CALLBACK_PATH_TOKEN=${PATH_TOKEN}}"

if aws_ lambda get-function --function-name "$FUNCTION_NAME" >/dev/null 2>&1; then
  aws_ lambda update-function-code --function-name "$FUNCTION_NAME" \
    --zip-file "fileb://$WORK_DIR/function.zip" >/dev/null
  aws_ lambda wait function-updated --function-name "$FUNCTION_NAME"
  aws_ lambda update-function-configuration --function-name "$FUNCTION_NAME" \
    --environment "Variables=${ENV_VARS}" >/dev/null
  aws_ lambda wait function-updated --function-name "$FUNCTION_NAME"
else
  aws_ lambda create-function --function-name "$FUNCTION_NAME" \
    --runtime python3.13 --architectures arm64 \
    --role "$LAMBDA_ROLE_ARN" --handler handler.handler \
    --zip-file "fileb://$WORK_DIR/function.zip" \
    --timeout 15 --memory-size 256 \
    --description "OAuth2 session-binding endpoint for lark-cli MCP (AgentCore Identity hop 2)" \
    --environment "Variables=${ENV_VARS}" >/dev/null
  aws_ lambda wait function-active-v2 --function-name "$FUNCTION_NAME"
fi
echo "function ${FUNCTION_NAME} deployed"

# AWS_IAM, not NONE: only CloudFront's OAC-signed requests should get through,
# and the resource policy below is what names it.
FUNCTION_URL=$(aws_ lambda get-function-url-config --function-name "$FUNCTION_NAME" \
  --query FunctionUrl --output text 2>/dev/null || \
  aws_ lambda create-function-url-config --function-name "$FUNCTION_NAME" \
    --auth-type AWS_IAM --query FunctionUrl --output text)
aws_ lambda update-function-url-config --function-name "$FUNCTION_NAME" \
  --auth-type AWS_IAM >/dev/null
ORIGIN_DOMAIN=$(printf '%s' "$FUNCTION_URL" | sed -E 's#https://([^/]+)/?#\1#')

# ── Origin access control ─────────────────────────────────────────────────────

OAC_ID=$(cf_ cloudfront list-origin-access-controls \
  --query "OriginAccessControlList.Items[?Name=='${OAC_NAME}'].Id | [0]" --output text 2>/dev/null || echo None)
if [ "$OAC_ID" = "None" ] || [ -z "$OAC_ID" ]; then
  OAC_ID=$(cf_ cloudfront create-origin-access-control --origin-access-control-config "{
      \"Name\": \"${OAC_NAME}\",
      \"Description\": \"OAC for the lark-cli OAuth session-binding Lambda function URL\",
      \"SigningProtocol\": \"sigv4\",
      \"SigningBehavior\": \"always\",
      \"OriginAccessControlOriginType\": \"lambda\"
    }" --query 'OriginAccessControl.Id' --output text)
fi
echo "origin access control: ${OAC_ID}"

# ── WAF ───────────────────────────────────────────────────────────────────────
#
# Scope CLOUDFRONT web ACLs are global and only manageable from us-east-1.

WAF_ARN=$(cf_ wafv2 list-web-acls --scope CLOUDFRONT \
  --query "WebACLs[?Name=='${WAF_NAME}'].ARN | [0]" --output text 2>/dev/null || echo None)
if [ "$WAF_ARN" = "None" ] || [ -z "$WAF_ARN" ]; then
  cat > "$WORK_DIR/waf-rules.json" <<'JSON'
[
  {
    "Name": "RateLimit",
    "Priority": 0,
    "Statement": {"RateBasedStatement": {"Limit": 100, "AggregateKeyType": "IP"}},
    "Action": {"Block": {}},
    "VisibilityConfig": {"SampledRequestsEnabled": true, "CloudWatchMetricsEnabled": true, "MetricName": "rateLimit"}
  },
  {
    "Name": "AWSManagedCommon",
    "Priority": 1,
    "Statement": {"ManagedRuleGroupStatement": {"VendorName": "AWS", "Name": "AWSManagedRulesCommonRuleSet"}},
    "OverrideAction": {"None": {}},
    "VisibilityConfig": {"SampledRequestsEnabled": true, "CloudWatchMetricsEnabled": true, "MetricName": "awsCommon"}
  },
  {
    "Name": "AWSManagedKnownBadInputs",
    "Priority": 2,
    "Statement": {"ManagedRuleGroupStatement": {"VendorName": "AWS", "Name": "AWSManagedRulesKnownBadInputsRuleSet"}},
    "OverrideAction": {"None": {}},
    "VisibilityConfig": {"SampledRequestsEnabled": true, "CloudWatchMetricsEnabled": true, "MetricName": "awsKnownBad"}
  }
]
JSON
  WAF_ARN=$(cf_ wafv2 create-web-acl --name "$WAF_NAME" --scope CLOUDFRONT \
    --default-action '{"Allow":{}}' \
    --visibility-config '{"SampledRequestsEnabled":true,"CloudWatchMetricsEnabled":true,"MetricName":"larkSessionBindingWaf"}' \
    --rules "file://$WORK_DIR/waf-rules.json" \
    --query 'Summary.ARN' --output text)
fi
echo "web acl: ${WAF_ARN}"

# ── Distribution ──────────────────────────────────────────────────────────────
#
# CachePolicyId          = Managed-CachingDisabled
# OriginRequestPolicyId  = Managed-AllViewerExceptHostHeader
# Both are required here: the callback carries single-use query parameters that
# must never be cached, and the origin must not receive the CloudFront Host
# header or the function URL rejects the signature.

DIST_ID=$(cf_ cloudfront list-distributions \
  --query "DistributionList.Items[?Origins.Items[0].DomainName=='${ORIGIN_DOMAIN}'].Id | [0]" \
  --output text 2>/dev/null || echo None)

if [ "$DIST_ID" = "None" ] || [ -z "$DIST_ID" ]; then
  cat > "$WORK_DIR/cf-config.json" <<JSON
{
  "CallerReference": "${FUNCTION_NAME}-${ACCOUNT}",
  "Comment": "Fronts the lark-cli OAuth session-binding Lambda; OAC restricts the function URL to this distribution",
  "Enabled": true,
  "WebACLId": "${WAF_ARN}",
  "Origins": {
    "Quantity": 1,
    "Items": [{
      "Id": "lambda-session-binding",
      "DomainName": "${ORIGIN_DOMAIN}",
      "OriginAccessControlId": "${OAC_ID}",
      "CustomOriginConfig": {
        "HTTPPort": 80,
        "HTTPSPort": 443,
        "OriginProtocolPolicy": "https-only",
        "OriginSslProtocols": {"Quantity": 1, "Items": ["TLSv1.2"]},
        "OriginReadTimeout": 30,
        "OriginKeepaliveTimeout": 5
      }
    }]
  },
  "DefaultCacheBehavior": {
    "TargetOriginId": "lambda-session-binding",
    "ViewerProtocolPolicy": "https-only",
    "AllowedMethods": {
      "Quantity": 2, "Items": ["GET", "HEAD"],
      "CachedMethods": {"Quantity": 2, "Items": ["GET", "HEAD"]}
    },
    "Compress": true,
    "CachePolicyId": "4135ea2d-6df8-44a3-9df3-4b5a84be39ad",
    "OriginRequestPolicyId": "b689b0a8-53d0-40ab-baf2-68738e2966ac"
  },
  "PriceClass": "PriceClass_100"
}
JSON
  DIST_ID=$(cf_ cloudfront create-distribution --distribution-config "file://$WORK_DIR/cf-config.json" \
    --query 'Distribution.Id' --output text)
fi
DIST_ARN="arn:aws:cloudfront::${ACCOUNT}:distribution/${DIST_ID}"
DIST_DOMAIN=$(cf_ cloudfront get-distribution --id "$DIST_ID" --query 'Distribution.DomainName' --output text)
echo "distribution: ${DIST_ID} (${DIST_DOMAIN})"

# ── Lock the function URL to this distribution ────────────────────────────────
#
# Both actions are needed: since October 2025 a function URL requires
# lambda:InvokeFunctionUrl *and* lambda:InvokeFunction. Named principal plus
# SourceArn, never a wildcard — that is the difference between "public endpoint"
# and "CloudFront-only origin" as far as both IAM and security scanners are
# concerned.

for SID in FunctionURLAllowPublicAccess FunctionURLInvokeAllowPublicAccess; do
  aws_ lambda remove-permission --function-name "$FUNCTION_NAME" --statement-id "$SID" >/dev/null 2>&1 || true
done
aws_ lambda remove-permission --function-name "$FUNCTION_NAME" --statement-id AllowCloudFrontOACInvokeUrl >/dev/null 2>&1 || true
aws_ lambda remove-permission --function-name "$FUNCTION_NAME" --statement-id AllowCloudFrontOACInvoke >/dev/null 2>&1 || true

aws_ lambda add-permission --function-name "$FUNCTION_NAME" \
  --statement-id AllowCloudFrontOACInvokeUrl \
  --action lambda:InvokeFunctionUrl --principal cloudfront.amazonaws.com \
  --source-arn "$DIST_ARN" --function-url-auth-type AWS_IAM >/dev/null
aws_ lambda add-permission --function-name "$FUNCTION_NAME" \
  --statement-id AllowCloudFrontOACInvoke \
  --action lambda:InvokeFunction --principal cloudfront.amazonaws.com \
  --source-arn "$DIST_ARN" --invoked-via-function-url >/dev/null
echo "function URL locked to ${DIST_ID}"

RETURN_URL="https://${DIST_DOMAIN}/${PATH_TOKEN}"

# ── Allowlist the return URL ───────────────────────────────────────────────────
#
# The loopback entry stays so local development, which uses the sidecar's own
# callback listener, keeps working against the same workload identity.
aws_ bedrock-agentcore-control update-workload-identity --name "$WORKLOAD_NAME" \
  --allowed-resource-oauth2-return-urls "$RETURN_URL" "http://localhost:3000/callback" >/dev/null
echo "return URL registered on ${WORKLOAD_NAME}"

cat <<EOF

Done. A new distribution takes 5-10 minutes to reach Deployed; poll with:

  aws cloudfront get-distribution --id ${DIST_ID} --query 'Distribution.Status'

Then set these on the AgentCore runtime and re-run the authorization:

  AGENTCORE_RETURN_URL=${RETURN_URL}
  AGENTCORE_STATE_SECRET=${STATE_SECRET}

Both are required. With only the return URL set, the sidecar sends no state and
every callback is rejected.

  aws bedrock-agentcore-control update-agent-runtime \\
    --agent-runtime-id <runtime-id> --region ${REGION} \\
    --environment-variables "AGENTCORE_RETURN_URL=${RETURN_URL},AGENTCORE_STATE_SECRET=${STATE_SECRET}" \\
    ... # plus the other required update-agent-runtime arguments

Verify: call lark_auth_start, complete the login, then call lark_exec directly —
lark_auth_complete should no longer be needed. A request straight to the function
URL should return 403, since only CloudFront can reach it.
EOF
