# Copyright (c) 2026 panlm
# SPDX-License-Identifier: MIT

"""OAuth 2.0 session-binding endpoint for the lark-cli MCP wrapper.

AgentCore Identity's authorization-code flow has two distinct redirect targets,
and this is the second one:

  1. Feishu -> AgentCore Identity's own callback URL (AWS-hosted, registered as
     the redirect URI on the Feishu app). Carries authorizationCode and state.
  2. AgentCore Identity -> this endpoint, the "session binding URL". Carries
     session_id (and the customState the sidecar signed).

Only hop 2 is the customer's responsibility. AWS documents that plainly: when
an agent runs on AgentCore Runtime, "your web application that connects to the
agent runtime must host a publicly accessible HTTPS callback endpoint itself"
(oauth2-authorization-url-session-binding.html). The two URLs are not
interchangeable — pointing the return URL at hop 1 makes the browser land on an
endpoint that demands authorizationCode and state and rejects the visit.

Cross-account by design: this function runs in the operator's own account, while
the Token Vault lives in the AgentCore account. Token vaults carry no resource
policy, so CompleteResourceTokenAuth cannot be granted cross-account directly —
this function assumes a narrowly scoped role in the AgentCore account instead.

Defence in depth, given that a Function URL serving browser redirects has to be
AuthType NONE (a redirect cannot carry SigV4):

  * signed customState — only the sidecar holds the HMAC key, so a request
    without a valid, unexpired state is rejected before any AWS call
  * unguessable path segment — keeps opportunistic scans of the lambda-url
    namespace from reaching the handler at all
  * least privilege — the assumed role can only call CompleteResourceTokenAuth,
    for one workload identity and one credential provider. A full compromise of
    this function still cannot read a token or start an authorization.
  * short-lived material — AgentCore expires each authorization URL and session
    URI after 10 minutes

What this cannot do: the AWS sample verifies that the user who started the
authorization is the user completing it, by reading a browser session cookie.
This deployment serves a single fixed userId and has no web session to check, so
that verification is replaced by the signed state above rather than implemented.
"""

import hashlib
import hmac
import json
import logging
import os
import time
from base64 import urlsafe_b64decode

import boto3

logger = logging.getLogger()
logger.setLevel(logging.INFO)

STATE_TTL_SECONDS = 600  # AgentCore expires the session URI after 10 minutes.

REGION = os.environ.get("AGENTCORE_REGION", "us-east-1")
USER_ID = os.environ["AGENTCORE_USER_ID"]
ASSUME_ROLE_ARN = os.environ["AGENTCORE_ASSUME_ROLE_ARN"]
EXTERNAL_ID = os.environ["AGENTCORE_EXTERNAL_ID"]
STATE_SECRET = os.environ["AGENTCORE_STATE_SECRET"].encode()
PATH_TOKEN = os.environ["CALLBACK_PATH_TOKEN"]

_sts = boto3.client("sts", region_name=REGION)


def _verify_state(state: str, now: float) -> None:
    """Validate a customState minted by the sidecar's signState.

    Format: v1.<unix-seconds>.<base64url HMAC-SHA256(secret, "v1." + seconds)>.
    The timestamp is part of the signed material, so it cannot be advanced
    without the key.
    """
    parts = (state or "").split(".")
    if len(parts) != 3 or parts[0] != "v1":
        raise ValueError("malformed state")
    payload = f"{parts[0]}.{parts[1]}".encode()
    expected = hmac.new(STATE_SECRET, payload, hashlib.sha256).digest()
    try:
        provided = urlsafe_b64decode(parts[2] + "=" * (-len(parts[2]) % 4))
    except Exception as exc:  # noqa: BLE001 - any decode failure is a bad state
        raise ValueError("malformed state signature") from exc
    if not hmac.compare_digest(provided, expected):
        raise ValueError("state signature mismatch")
    age = now - int(parts[1])
    if age < -60 or age > STATE_TTL_SECONDS:
        raise ValueError(f"state expired or not yet valid (age {age:.0f}s)")


def _complete_auth(session_id: str) -> None:
    """Bind the completed browser authorization to this deployment's userId.

    Runs against the AgentCore account via a scoped assumed role, since the
    Token Vault lives there and cannot be shared through a resource policy.
    """
    creds = _sts.assume_role(
        RoleArn=ASSUME_ROLE_ARN,
        RoleSessionName="lark-session-binding",
        ExternalId=EXTERNAL_ID,
        DurationSeconds=900,
    )["Credentials"]

    boto3.client(
        "bedrock-agentcore",
        region_name=REGION,
        aws_access_key_id=creds["AccessKeyId"],
        aws_secret_access_key=creds["SecretAccessKey"],
        aws_session_token=creds["SessionToken"],
    ).complete_resource_token_auth(
        sessionUri=session_id,
        userIdentifier={"userId": USER_ID},
    )


def _page(title: str, message: str) -> str:
    return (
        "<!doctype html><meta charset=utf-8>"
        "<meta name=viewport content='width=device-width,initial-scale=1'>"
        f"<title>{title}</title>"
        "<div style=\"font:16px/1.6 -apple-system,BlinkMacSystemFont,"
        'sans-serif;max-width:34rem;margin:20vh auto;padding:0 1.5rem">'
        f"<h1 style=font-size:1.4rem>{title}</h1><p>{message}</p></div>"
    )


def _html(status: int, title: str, message: str) -> dict:
    return {
        "statusCode": status,
        "headers": {
            "Content-Type": "text/html; charset=utf-8",
            "Cache-Control": "no-store",
            # This page ends an OAuth flow and is never framed or scripted.
            "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'",
            "X-Content-Type-Options": "nosniff",
            "Referrer-Policy": "no-referrer",
        },
        "body": _page(title, message),
    }


def handler(event, _context):
    path = event.get("rawPath", "")
    params = event.get("queryStringParameters") or {}

    # Wrong path: answer exactly as if nothing is deployed here, so scanning the
    # lambda-url namespace reveals nothing about what this function is.
    if not path.endswith(f"/{PATH_TOKEN}"):
        logger.warning("rejected request to unexpected path")
        return _html(404, "Not found", "No such page.")

    session_id = params.get("session_id")
    if not session_id:
        logger.warning("callback without session_id")
        return _html(400, "Missing session", "This link is missing its session_id.")

    try:
        _verify_state(params.get("state", ""), time.time())
    except ValueError as exc:
        # Deliberately vague to the browser; the reason goes to CloudWatch only.
        logger.warning("state rejected: %s", exc)
        return _html(400, "Invalid request", "This authorization link is not valid.")

    try:
        _complete_auth(session_id)
    except Exception:  # noqa: BLE001 - report one generic failure to the browser
        logger.exception("CompleteResourceTokenAuth failed")
        return _html(
            502,
            "Authorization failed",
            "The login could not be completed. Please start the authorization again.",
        )

    logger.info("session bound for userId=%s", USER_ID)
    return _html(
        200,
        "Feishu authorization complete",
        "You can close this tab and return to your agent session.",
    )
