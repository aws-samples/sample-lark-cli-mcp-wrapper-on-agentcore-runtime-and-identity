package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeDownloader installs a stand-in lark-cli that behaves like
// `im +messages-resources-download`: it writes the bytes of src to the
// --output path (relative to its working directory), appends its argv to a
// log file, and prints the JSON envelope the real command prints. It returns
// the path of the argv log so tests can count invocations and inspect flags.
func fakeDownloader(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write fake source: %v", err)
	}
	argLog := filepath.Join(dir, "argv.log")
	bin := writeFakeBinary(t, `
out=""
for a in "$@"; do
  if [ "$prev" = "--output" ]; then out="$a"; fi
  prev="$a"
done
echo "$@" >> "`+argLog+`"
cat "`+src+`" > "$out"
printf '{"ok":true,"data":{"saved_path":"%s/%s"}}' "$(pwd)" "$out"
`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)
	// Keep cached downloads out of the real temp directory; tests that
	// inspect the cache override TMPDIR again with their own root.
	t.Setenv("TMPDIR", t.TempDir())
	return argLog
}

func invocations(t *testing.T, argLog string) []string {
	t.Helper()
	b, err := os.ReadFile(argLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("failed to read argv log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func validRequest(chunk int) downloadRequest {
	return downloadRequest{MessageID: "om_x100b", FileKey: "file_v3_abc", Type: "file", FileName: "report.xlsx", Chunk: chunk}
}

func TestDownload_SmallFileComesBackWhole(t *testing.T) {
	fakeDownloader(t, "hello")

	got, err := newDownloader().fetch(context.Background(), validRequest(0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got.Data) != "hello" {
		t.Errorf("data = %q, want %q", got.Data, "hello")
	}
	if got.Chunks != 1 || got.TotalSize != 5 {
		t.Errorf("chunks=%d total=%d, want 1 and 5", got.Chunks, got.TotalSize)
	}
	// sha256("hello"), computed independently with `printf hello | shasum -a 256`.
	if got.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("sha256 = %s", got.SHA256)
	}
}

func TestDownload_PassesResourceFlagsToLarkCLI(t *testing.T) {
	argLog := fakeDownloader(t, "x")

	if _, err := newDownloader().fetch(context.Background(), downloadRequest{MessageID: "om_1", FileKey: "img_v3_2", Type: "image"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := invocations(t, argLog)
	if len(calls) != 1 {
		t.Fatalf("lark-cli ran %d times, want 1", len(calls))
	}
	for _, want := range []string{"im +messages-resources-download", "--message-id om_1", "--file-key img_v3_2", "--type image", "--output "} {
		if !strings.Contains(calls[0], want) {
			t.Errorf("argv %q missing %q", calls[0], want)
		}
	}
}

func TestDownload_LargeFileIsSplitIntoChunks(t *testing.T) {
	fakeDownloader(t, "abcdefghij")
	t.Setenv("LARK_DOWNLOAD_CHUNK_BYTES", "4")
	d := newDownloader()

	want := []string{"abcd", "efgh", "ij"}
	for i, w := range want {
		got, err := d.fetch(context.Background(), validRequest(i))
		if err != nil {
			t.Fatalf("chunk %d: unexpected error: %v", i, err)
		}
		if string(got.Data) != w {
			t.Errorf("chunk %d = %q, want %q", i, got.Data, w)
		}
		if got.Chunks != 3 || got.Chunk != i {
			t.Errorf("chunk %d reports chunk=%d chunks=%d, want %d and 3", i, got.Chunk, got.Chunks, i)
		}
	}
}

// Later chunks must be cut from the copy already in the container rather than
// pulling the whole file from Feishu again for every chunk.
func TestDownload_LaterChunksReuseTheDownloadedFile(t *testing.T) {
	argLog := fakeDownloader(t, "abcdefghij")
	t.Setenv("LARK_DOWNLOAD_CHUNK_BYTES", "4")
	d := newDownloader()

	for i := 0; i < 2; i++ {
		if _, err := d.fetch(context.Background(), validRequest(i)); err != nil {
			t.Fatalf("chunk %d: unexpected error: %v", i, err)
		}
	}
	if n := len(invocations(t, argLog)); n != 1 {
		t.Errorf("lark-cli ran %d times for two chunks, want 1", n)
	}
}

// A later chunk can land on a different microVM (or after the cache was
// swept), where nothing is cached: it must re-download instead of failing.
func TestDownload_ChunkWithoutCacheRedownloads(t *testing.T) {
	argLog := fakeDownloader(t, "abcdefghij")
	t.Setenv("LARK_DOWNLOAD_CHUNK_BYTES", "4")

	got, err := newDownloader().fetch(context.Background(), validRequest(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got.Data) != "ij" {
		t.Errorf("data = %q, want %q", got.Data, "ij")
	}
	if n := len(invocations(t, argLog)); n != 1 {
		t.Errorf("lark-cli ran %d times, want 1", n)
	}
}

func TestDownload_ServingLastChunkRemovesTheContainerCopy(t *testing.T) {
	fakeDownloader(t, "abcdefghij")
	t.Setenv("LARK_DOWNLOAD_CHUNK_BYTES", "4")
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	d := newDownloader()

	if _, err := d.fetch(context.Background(), validRequest(0)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if left := filesUnder(t, root); len(left) != 1 {
		t.Fatalf("after first chunk, files in container temp = %v, want exactly the cached file", left)
	}
	if _, err := d.fetch(context.Background(), validRequest(2)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if left := filesUnder(t, root); len(left) != 0 {
		t.Errorf("after last chunk, files left in container temp = %v, want none", left)
	}
}

func TestDownload_IdleCacheEntriesAreSwept(t *testing.T) {
	fakeDownloader(t, "abcdefghij")
	t.Setenv("LARK_DOWNLOAD_CHUNK_BYTES", "4")
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	d := newDownloader()
	now := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }

	if _, err := d.fetch(context.Background(), validRequest(0)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	now = now.Add(16 * time.Minute)
	other := downloadRequest{MessageID: "om_other", FileKey: "file_v3_other", Type: "file"}
	if _, err := d.fetch(context.Background(), other); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Both files span 3 chunks, so neither is removed by being fully served:
	// only the sweep can drop the first one, leaving just the second.
	if left := filesUnder(t, root); len(left) != 1 {
		t.Errorf("files in container temp after 16 idle minutes = %v, want only the second download", left)
	}
}

func TestDownload_RejectsFileOverTheSizeLimit(t *testing.T) {
	fakeDownloader(t, "abcdef")
	t.Setenv("LARK_DOWNLOAD_MAX_BYTES", "5")
	root := t.TempDir()
	t.Setenv("TMPDIR", root)

	_, err := newDownloader().fetch(context.Background(), validRequest(0))
	if err == nil {
		t.Fatal("expected error for a 6-byte file with a 5-byte limit, got nil")
	}
	if left := filesUnder(t, root); len(left) != 0 {
		t.Errorf("oversize file left behind: %v", left)
	}
}

func TestDownload_RejectsChunkPastTheEnd(t *testing.T) {
	fakeDownloader(t, "abcdefghij")
	t.Setenv("LARK_DOWNLOAD_CHUNK_BYTES", "4")

	if _, err := newDownloader().fetch(context.Background(), validRequest(3)); err == nil {
		t.Fatal("expected error for chunk 3 of a 3-chunk file, got nil")
	}
}

// message_id and file_key reach lark-cli as argv; a value starting with "--"
// would be parsed as an extra flag, so malformed IDs are refused before
// lark-cli runs at all.
func TestDownload_RejectsMalformedIDsWithoutRunningLarkCLI(t *testing.T) {
	argLog := fakeDownloader(t, "x")
	tests := []struct {
		name string
		req  downloadRequest
	}{
		{"flag-like file_key", downloadRequest{MessageID: "om_1", FileKey: "--as", Type: "file"}},
		{"message_id without om_ prefix", downloadRequest{MessageID: "x_1", FileKey: "file_v3_1", Type: "file"}},
		{"empty file_key", downloadRequest{MessageID: "om_1", FileKey: "", Type: "file"}},
		{"unknown type", downloadRequest{MessageID: "om_1", FileKey: "file_v3_1", Type: "video"}},
		{"negative chunk", downloadRequest{MessageID: "om_1", FileKey: "file_v3_1", Type: "file", Chunk: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newDownloader().fetch(context.Background(), tc.req); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
	if n := len(invocations(t, argLog)); n != 0 {
		t.Errorf("lark-cli ran %d times for invalid requests, want 0", n)
	}
}

func TestDownload_LarkCLIFailureIsReported(t *testing.T) {
	bin := writeFakeBinary(t, `echo "permission denied: im:resource" >&2; exit 1`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	_, err := newDownloader().fetch(context.Background(), validRequest(0))
	if err == nil {
		t.Fatal("expected error when lark-cli exits non-zero, got nil")
	}
	if !strings.Contains(err.Error(), "permission denied: im:resource") {
		t.Errorf("error %q does not carry lark-cli's stderr", err)
	}
}

// The binary content goes back as an embedded resource blob: Claude Code (and
// other MCP clients) write blob resources to a local file instead of feeding
// the bytes to the model.
func TestDownloadHandler_ReturnsChunkAsEmbeddedBlob(t *testing.T) {
	fakeDownloader(t, "hello")
	handler := newLarkDownloadHandler(newDownloader())

	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, larkDownloadArgs{
		MessageID: "om_x100b", FileKey: "file_v3_abc", Type: "file", FileName: "report.xlsx",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError = true, content = %+v", res.Content)
	}
	var blob *mcp.EmbeddedResource
	for _, c := range res.Content {
		if e, ok := c.(*mcp.EmbeddedResource); ok {
			blob = e
		}
	}
	if blob == nil || blob.Resource == nil {
		t.Fatalf("no embedded resource in result: %+v", res.Content)
	}
	if string(blob.Resource.Blob) != "hello" {
		t.Errorf("blob = %q, want %q", blob.Resource.Blob, "hello")
	}
	if !strings.HasSuffix(blob.Resource.URI, "/report.xlsx") {
		t.Errorf("uri = %q, want it to end with the file name", blob.Resource.URI)
	}
	if blob.Resource.MIMEType != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Errorf("mime = %q", blob.Resource.MIMEType)
	}
}

// Claude Code turns an image/* blob into a re-encoded JPEG preview instead of
// saving the original bytes, so image blobs are labelled as opaque binary and
// the real type travels only in the metadata.
func TestDownloadHandler_ImageBlobIsSentAsOpaqueBinary(t *testing.T) {
	fakeDownloader(t, "\x89PNG\r\n\x1a\nrest-of-png")
	handler := newLarkDownloadHandler(newDownloader())

	res, meta, err := handler(context.Background(), &mcp.CallToolRequest{}, larkDownloadArgs{
		MessageID: "om_1", FileKey: "img_v3_1", Type: "image", FileName: "shot.png",
	})
	if err != nil || res.IsError {
		t.Fatalf("unexpected failure: err=%v content=%+v", err, res.Content)
	}
	for _, c := range res.Content {
		if e, ok := c.(*mcp.EmbeddedResource); ok && e.Resource.MIMEType != "application/octet-stream" {
			t.Errorf("image blob mime = %q, want application/octet-stream", e.Resource.MIMEType)
		}
	}
	if meta.MIMEType != "image/png" {
		t.Errorf("metadata mime = %q, want image/png", meta.MIMEType)
	}
}

func TestDownloadHandler_FailureIsAToolErrorNotAProtocolError(t *testing.T) {
	bin := writeFakeBinary(t, `echo "boom" >&2; exit 2`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)
	handler := newLarkDownloadHandler(newDownloader())

	res, _, err := handler(context.Background(), &mcp.CallToolRequest{}, larkDownloadArgs{
		MessageID: "om_1", FileKey: "file_v3_1", Type: "file",
	})
	if err != nil {
		t.Fatalf("handler returned protocol error %v, want a tool result with IsError", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false for a failed download")
	}
}

func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
