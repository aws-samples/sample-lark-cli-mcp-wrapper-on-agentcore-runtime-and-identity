package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// larkExecArgs is the input schema for the lark_exec tool, inferred by the
// SDK from this struct's fields and jsonschema tags.
type larkExecArgs struct {
	Command string `json:"command" jsonschema:"Arguments to pass to lark-cli, space-separated"`
}

// larkExecHandler adapts runLarkExec to the SDK's typed tool handler shape.
// IsError reflects whether the lark-cli invocation itself exited non-zero,
// not whether the MCP call succeeded — a non-zero exit is a normal (if
// unsuccessful) command result, not a protocol-level error. An error is
// returned from this handler only when the command could not be run at all
// (e.g. bad quoting), which the SDK surfaces as a tool error automatically.
func larkExecHandler(ctx context.Context, req *mcp.CallToolRequest, args larkExecArgs) (*mcp.CallToolResult, execResult, error) {
	result, err := runLarkExec(ctx, args.Command)
	if err != nil {
		return nil, execResult{}, err
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return nil, execResult{}, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
		IsError: result.ExitCode != 0,
	}, result, nil
}

// larkListSkillsArgs is the input schema for the lark_list_skills tool. It
// takes no parameters.
type larkListSkillsArgs struct{}

// larkListSkillsHandler adapts runLarkListSkills to the SDK's typed tool
// handler shape, following the same IsError convention as larkExecHandler.
func larkListSkillsHandler(ctx context.Context, req *mcp.CallToolRequest, args larkListSkillsArgs) (*mcp.CallToolResult, execResult, error) {
	result, err := runLarkListSkills(ctx)
	if err != nil {
		return nil, execResult{}, err
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return nil, execResult{}, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
		IsError: result.ExitCode != 0,
	}, result, nil
}

// larkGetSkillArgs is the input schema for the lark_get_skill tool, inferred
// by the SDK from this struct's fields and jsonschema tags.
type larkGetSkillArgs struct {
	SkillName string `json:"skill_name" jsonschema:"Name of the Skill, as returned by lark_list_skills"`
}

// larkGetSkillHandler adapts runLarkGetSkill to the SDK's typed tool handler
// shape, following the same IsError convention as larkExecHandler.
func larkGetSkillHandler(ctx context.Context, req *mcp.CallToolRequest, args larkGetSkillArgs) (*mcp.CallToolResult, execResult, error) {
	result, err := runLarkGetSkill(ctx, args.SkillName)
	if err != nil {
		return nil, execResult{}, err
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return nil, execResult{}, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
		IsError: result.ExitCode != 0,
	}, result, nil
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "lark-cli-mcp-wrapper", Version: "0.1.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "lark_exec",
		Description: "Execute a lark-cli command and return its stdout/stderr/exit code. Pass the full command line as it would appear after 'lark-cli '. Wrap any argument containing spaces in double or single quotes, e.g. 'im +messages-send --as user --receive-id ou_xxx --receive-id-type open_id --text \"hello world\"'.",
	}, larkExecHandler)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "lark_download",
		Description: "Download an image or file attached to a Feishu message and return its bytes as an embedded binary resource (MCP clients such as Claude Code save it to a local file). Take message_id and the img_/file_ key from the message content (e.g. im +chat-messages-list), and pass the original file name if known. Large files come back in chunks: if the result reports chunks > 1, call again with chunk=1 .. chunks-1, concatenate the saved chunk files in order, and check the result against sha256 and total_size. Files over 100 MB are refused.",
	}, newLarkDownloadHandler(newDownloader()))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "lark_list_skills",
		Description: "List all available lark-cli Skills (structured multi-step operation guides for common Feishu/Lark tasks like scheduling meetings or searching messages).",
	}, larkListSkillsHandler)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "lark_get_skill",
		Description: "Fetch the detailed orchestration guide for one Skill by name (parameter formats, call order, preconditions). Call lark_list_skills first to discover available names.",
	}, larkGetSkillHandler)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "lark_auth_start",
		Description: "Begin Feishu authorization when lark_exec reports that authorization is required. Returns a URL for the operator to open in a browser, plus the session_id needed to finish. Takes no parameters. Follow this with lark_auth_complete.",
	}, larkAuthStartHandler)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "lark_auth_complete",
		Description: "Finish the authorization started by lark_auth_start, binding the operator's completed browser login to the credential vault. Pass the session_id from lark_auth_start (or from the session_id query parameter on the post-login redirect URL). Call within about 5 minutes of the login.",
	}, larkAuthCompleteHandler)

	// DisableLocalhostProtection: AgentCore Runtime's internal proxy connects
	// to this container over its own loopback interface while forwarding the
	// original public Host header, which otherwise trips the SDK's DNS
	// rebinding guard (a mismatch between a loopback local address and a
	// non-loopback Host header) with an unconditional 403. AgentCore Runtime
	// already provides network isolation per container, so this guard is
	// redundant there; local dev keeps the default protection since it's the
	// scenario the guard exists for.
	opts := &mcp.StreamableHTTPOptions{DisableLocalhostProtection: os.Getenv("AGENTCORE_RUNTIME") != ""}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, opts)

	http.Handle("/mcp", handler)
	listen := listenAddr()
	log.Printf("MCP wrapper listening on %s/mcp\n", listen)
	if err := http.ListenAndServe(listen, nil); err != nil {
		log.Fatal(err)
	}
}

// listenAddr returns the address to bind. AgentCore Runtime requires MCP
// containers to serve on 0.0.0.0:8000/mcp; MCP_WRAPPER_LISTEN overrides this
// for local development (defaults to loopback-only 127.0.0.1:8090 there).
func listenAddr() string {
	if v := os.Getenv("MCP_WRAPPER_LISTEN"); v != "" {
		return v
	}
	if os.Getenv("AGENTCORE_RUNTIME") != "" {
		return "0.0.0.0:8000"
	}
	return "127.0.0.1:8090"
}
