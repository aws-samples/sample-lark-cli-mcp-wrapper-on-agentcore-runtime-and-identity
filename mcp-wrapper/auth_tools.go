package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sidecarBaseURL returns the base URL of the AgentCore Identity sidecar's
// management surface. The sidecar runs in this same container on loopback and
// entrypoint.sh already exports its address as LARKSUITE_CLI_AUTH_PROXY for
// lark-cli's benefit, so we reuse it rather than introducing a second knob.
func sidecarBaseURL() string {
	if v := os.Getenv("LARKSUITE_CLI_AUTH_PROXY"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:16385"
}

// postSidecar POSTs body to one of the sidecar's /_sidecar/auth/* management
// endpoints and decodes the JSON response. Those endpoints are deliberately
// not HMAC-verified (unlike the data-plane proxy): they are loopback-only, and
// this wrapper shares the container's loopback interface with the sidecar, so
// it is inside the same trust boundary.
func postSidecar(ctx context.Context, path string, body any) (map[string]any, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to encode request body: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sidecarBaseURL()+path, payload)
	if err != nil {
		return nil, fmt.Errorf("failed to build sidecar request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach sidecar at %s: %w", sidecarBaseURL()+path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read sidecar response: %w", err)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("sidecar returned non-JSON response (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if ok, _ := out["ok"].(bool); !ok {
		msg, _ := out["error"].(string)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return nil, fmt.Errorf("sidecar %s failed (HTTP %d): %s", path, resp.StatusCode, msg)
	}
	return out, nil
}

// splitAuthURL decomposes an AgentCore Identity authorization URL into the
// pieces a human needs to reassemble it by hand.
//
// Why bother: the DevOps Agent service redacts tool output that looks like a
// sensitive URL, replacing it with the literal string "<redacted>" before the
// model ever sees it (observed 2026-07-21 and again 2026-07-27). A URL is
// therefore not reliably deliverable to the operator through a chat transcript.
// Returning the same information several ways — full URL, scheme-stripped URL,
// and the bare request_uri — means at least one form survives the filter, so
// the operator always has something to copy into a browser.
func splitAuthURL(rawURL string) (noScheme, requestURI string) {
	noScheme = strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	if parsed, err := url.Parse(rawURL); err == nil {
		requestURI = parsed.Query().Get("request_uri")
	}
	return noScheme, requestURI
}

// larkAuthStartArgs is the input schema for the lark_auth_start tool. It takes
// no parameters: every identifier (workload, credential provider, user id,
// scopes, return URL) is fixed deployment configuration held by the sidecar.
type larkAuthStartArgs struct{}

// larkAuthStartHandler begins a fresh Feishu 3LO authorization and returns the
// authorization URL plus the session_id needed to finish the binding.
//
// The return URL registered for this deployment points at the sidecar's
// container-local loopback listener, which the operator's browser cannot
// reach. That is not a problem: AgentCore Identity appends session_id to the
// redirect, so the browser's address bar carries the value even when the page
// itself fails to load. The operator copies that session_id back and calls
// lark_auth_complete — completion only needs the session URI, not a reachable
// return URL.
func larkAuthStartHandler(ctx context.Context, req *mcp.CallToolRequest, args larkAuthStartArgs) (*mcp.CallToolResult, map[string]any, error) {
	out, err := postSidecar(ctx, "/_sidecar/auth/start", nil)
	if err != nil {
		return nil, nil, err
	}

	result := map[string]any{}
	if already, _ := out["already_authorized"].(bool); already {
		result["status"] = "already_authorized"
		result["next_step"] = "A valid Feishu token is already vaulted. No action needed; just call lark_exec."
	} else {
		authURL, _ := out["authorization_url"].(string)
		sessionURI, _ := out["session_uri"].(string)
		noScheme, requestURI := splitAuthURL(authURL)

		result["status"] = "authorization_required"
		result["authorization_url"] = authURL
		result["authorization_url_without_scheme"] = noScheme
		result["request_uri"] = requestURI
		result["session_id"] = sessionURI
		result["next_step"] = strings.Join([]string{
			"1. Open the authorization URL in a browser and complete the Feishu login.",
			"   If 'authorization_url' above reads '<redacted>', the transport filtered it:",
			"   take 'authorization_url_without_scheme' and prepend 'https://' instead.",
			"2. The browser will be redirected to a localhost address that fails to load.",
			"   That failure is expected. Copy the 'session_id' query parameter out of the",
			"   browser's address bar (it starts with 'urn:ietf:params:oauth:request_uri:').",
			"3. Call lark_auth_complete with that value within about 5 minutes,",
			"   since the underlying OAuth authorization code is short-lived.",
		}, "\n")
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
	}, result, nil
}

// larkAuthCompleteArgs is the input schema for the lark_auth_complete tool.
type larkAuthCompleteArgs struct {
	SessionID string `json:"session_id" jsonschema:"The session_id returned by lark_auth_start, also present as a query parameter on the post-login redirect URL. Starts with 'urn:ietf:params:oauth:request_uri:'."`
}

// larkAuthCompleteHandler binds a browser authorization completed by the
// operator to this deployment's vault slot, making the token available to
// subsequent lark_exec calls.
func larkAuthCompleteHandler(ctx context.Context, req *mcp.CallToolRequest, args larkAuthCompleteArgs) (*mcp.CallToolResult, map[string]any, error) {
	sessionID := strings.TrimSpace(args.SessionID)
	if sessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	// The value is frequently copied out of a browser address bar, where it
	// appears percent-encoded (urn%3Aietf%3A...). Decode it so either form
	// works, rather than failing with an opaque AWS-side error.
	if decoded, err := url.QueryUnescape(sessionID); err == nil {
		sessionID = decoded
	}

	if _, err := postSidecar(ctx, "/_sidecar/auth/complete", map[string]string{"session_id": sessionID}); err != nil {
		return nil, nil, err
	}

	result := map[string]any{
		"status":    "authorized",
		"next_step": "Token bound to the vault. Verify with lark_exec, e.g. 'im +chat-list --as user --page-size 3'.",
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
	}, result, nil
}
