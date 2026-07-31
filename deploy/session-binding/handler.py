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

Deployed as a Lambda behind CloudFront with origin access control, in the same
account as the Token Vault. Two consequences shape this file:

  * The function URL is AuthType AWS_IAM and its resource policy names only
    cloudfront.amazonaws.com for one distribution ARN, so a direct request to
    the function URL is refused and CloudFront is the only way in. A browser
    redirect cannot carry SigV4, which is why the public edge has to be
    unauthenticated — the checks below are what stands in for that.
  * Same account means no role assumption: the execution role holds
    CompleteResourceTokenAuth and the provider-secret read directly. A
    cross-account trust policy would work too but is worth avoiding, since a
    trust policy naming an external principal is exactly what internal security
    scanners flag.

Defence in depth at the edge:

  * signed customState — only the sidecar holds the HMAC key, so a request
    without a valid, unexpired state is rejected before any AWS call
  * unguessable path segment — anything else answers 404 and reveals nothing
  * WAF in front — rate limiting plus the AWS managed rule sets
  * least privilege — the execution role can only complete a session binding for
    one workload identity and one credential provider. A full compromise of this
    function still cannot read a token or start an authorization.
  * short-lived material — AgentCore expires each authorization URL and session
    URI after 10 minutes

What this cannot do: the AWS sample verifies that the user who started the
authorization is the user completing it, by reading a browser session cookie.
This deployment serves a single fixed userId and has no web session to check, so
that verification is replaced by the signed state above rather than implemented.
"""

import hashlib
import hmac
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
STATE_SECRET = os.environ["AGENTCORE_STATE_SECRET"].encode()
PATH_TOKEN = os.environ["CALLBACK_PATH_TOKEN"]

_agentcore = boto3.client("bedrock-agentcore", region_name=REGION)


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

    # Wrong path: answer exactly as if nothing is deployed here, so a scan of the
    # distribution reveals nothing about what this function is.
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
        _agentcore.complete_resource_token_auth(
            sessionUri=session_id,
            userIdentifier={"userId": USER_ID},
        )
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
