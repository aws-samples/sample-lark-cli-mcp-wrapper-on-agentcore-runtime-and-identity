package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Chunk and size defaults. AgentCore Gateway caps a tool-call response at
// 6 MB and the blob travels base64-encoded (4/3 larger), so 3 MiB of raw
// bytes per chunk leaves headroom for the JSON envelope. Feishu's message
// resource API and AgentCore Runtime both stop at 100 MB, so a bigger default
// limit would only fail later and less clearly.
const (
	defaultDownloadChunkBytes = 3 << 20
	defaultDownloadMaxBytes   = 100 << 20
	downloadIdleTTL           = 15 * time.Minute
	downloadTimeout           = 5 * time.Minute
)

var (
	messageIDPattern = regexp.MustCompile(`^om_[A-Za-z0-9_-]+$`)
	fileKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// downloadRequest names one message resource and which chunk of it to return.
type downloadRequest struct {
	MessageID string
	FileKey   string
	Type      string
	FileName  string
	Chunk     int
}

// downloadChunk is one slice of a downloaded resource plus what the caller
// needs to reassemble and verify the whole file.
type downloadChunk struct {
	FileName  string `json:"file_name"`
	MIMEType  string `json:"mime_type"`
	TotalSize int64  `json:"total_size"`
	SHA256    string `json:"sha256"`
	Chunk     int    `json:"chunk"`
	Chunks    int    `json:"chunks"`
	Data      []byte `json:"-"`
}

type cachedDownload struct {
	dir      string
	path     string
	size     int64
	sha256   string
	lastUsed time.Time
}

// downloader fetches message resources through lark-cli into a private
// temporary directory and serves them back in chunks. The cache only lives as
// long as this process (one AgentCore microVM); a chunk request that misses it
// simply downloads the file again.
type downloader struct {
	mu      sync.Mutex
	entries map[string]*cachedDownload
	now     func() time.Time
}

func newDownloader() *downloader {
	return &downloader{entries: map[string]*cachedDownload{}, now: time.Now}
}

func envBytes(name string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil && v > 0 {
		return v
	}
	return def
}

func (r downloadRequest) validate() error {
	if !messageIDPattern.MatchString(r.MessageID) {
		return fmt.Errorf("message_id %q is not a Feishu message ID (om_...)", r.MessageID)
	}
	if !fileKeyPattern.MatchString(r.FileKey) {
		return fmt.Errorf("file_key %q is not a Feishu resource key", r.FileKey)
	}
	if r.Type != "image" && r.Type != "file" {
		return fmt.Errorf("type must be \"image\" or \"file\", got %q", r.Type)
	}
	if r.Chunk < 0 {
		return fmt.Errorf("chunk must be >= 0, got %d", r.Chunk)
	}
	return nil
}

func (d *downloader) fetch(ctx context.Context, req downloadRequest) (downloadChunk, error) {
	if err := req.validate(); err != nil {
		return downloadChunk{}, err
	}
	chunkBytes := envBytes("LARK_DOWNLOAD_CHUNK_BYTES", defaultDownloadChunkBytes)
	key := req.MessageID + "/" + req.FileKey + "/" + req.Type

	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked()

	entry, ok := d.entries[key]
	if !ok {
		var err error
		entry, err = d.download(ctx, req)
		if err != nil {
			return downloadChunk{}, err
		}
		d.entries[key] = entry
	}
	entry.lastUsed = d.now()

	chunks := int((entry.size + chunkBytes - 1) / chunkBytes)
	if chunks == 0 {
		chunks = 1
	}
	if req.Chunk >= chunks {
		return downloadChunk{}, fmt.Errorf("chunk %d out of range: file has %d chunk(s)", req.Chunk, chunks)
	}
	data, err := readChunk(entry.path, int64(req.Chunk)*chunkBytes, chunkBytes)
	if err != nil {
		return downloadChunk{}, err
	}
	if req.Chunk == chunks-1 {
		os.RemoveAll(entry.dir)
		delete(d.entries, key)
	}

	name := displayName(req)
	return downloadChunk{
		FileName:  name,
		MIMEType:  detectMIME(name, data),
		TotalSize: entry.size,
		SHA256:    entry.sha256,
		Chunk:     req.Chunk,
		Chunks:    chunks,
		Data:      data,
	}, nil
}

// sweepLocked drops cached files nobody has asked for within downloadIdleTTL,
// so an abandoned multi-chunk transfer does not pin disk for the life of the
// microVM.
func (d *downloader) sweepLocked() {
	for k, e := range d.entries {
		if d.now().Sub(e.lastUsed) > downloadIdleTTL {
			os.RemoveAll(e.dir)
			delete(d.entries, k)
		}
	}
}

func (d *downloader) download(ctx context.Context, req downloadRequest) (*cachedDownload, error) {
	dir, err := os.MkdirTemp("", "lark-dl-")
	if err != nil {
		return nil, fmt.Errorf("create download dir: %w", err)
	}
	// lark-cli only accepts a relative --output, resolved against its working
	// directory, so it runs inside the private directory created above.
	const outName = "resource"
	args := []string{"im", "+messages-resources-download",
		"--message-id", req.MessageID, "--file-key", req.FileKey,
		"--type", req.Type, "--output", outName}
	res, err := runLarkCLI(ctx, dir, downloadTimeout, args)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if res.ExitCode != 0 {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("lark-cli exited %d: %s%s", res.ExitCode, res.Stderr, res.Stdout)
	}

	path := filepath.Join(dir, outName)
	var envelope struct {
		Data struct {
			SavedPath string `json:"saved_path"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(res.Stdout), &envelope) == nil && envelope.Data.SavedPath != "" {
		// Trust the reported path only if it stays inside our directory.
		if p, err := filepath.EvalSymlinks(envelope.Data.SavedPath); err == nil {
			if realDir, err := filepath.EvalSymlinks(dir); err == nil && filepath.Dir(p) == realDir {
				path = p
			}
		}
	}

	f, err := os.Open(path)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("downloaded file missing: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if limit := envBytes("LARK_DOWNLOAD_MAX_BYTES", defaultDownloadMaxBytes); info.Size() > limit {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("file is %d bytes, over the %d-byte download limit", info.Size(), limit)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &cachedDownload{dir: dir, path: path, size: info.Size(), sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

func readChunk(path string, offset, length int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// displayName is the caller-supplied file name reduced to its base, falling
// back to the resource key so the client still gets a stable name.
func displayName(req downloadRequest) string {
	if n := filepath.Base(req.FileName); req.FileName != "" && n != "." && n != "/" {
		return n
	}
	return req.FileKey
}

func detectMIME(name string, head []byte) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(head)
}

// larkDownloadArgs is the input schema for the lark_download tool.
type larkDownloadArgs struct {
	MessageID string `json:"message_id" jsonschema:"Message ID (om_...) that carries the image or file"`
	FileKey   string `json:"file_key" jsonschema:"Resource key from the message content: img_... for images, file_... for files"`
	Type      string `json:"type" jsonschema:"image or file"`
	FileName  string `json:"file_name,omitempty" jsonschema:"Optional original file name (from the message content), used for the returned resource name and MIME type"`
	Chunk     int    `json:"chunk,omitempty" jsonschema:"0-based chunk index; omit for the first chunk"`
}

func newLarkDownloadHandler(d *downloader) func(context.Context, *mcp.CallToolRequest, larkDownloadArgs) (*mcp.CallToolResult, downloadChunk, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args larkDownloadArgs) (*mcp.CallToolResult, downloadChunk, error) {
		chunk, err := d.fetch(ctx, downloadRequest{
			MessageID: args.MessageID, FileKey: args.FileKey, Type: args.Type,
			FileName: args.FileName, Chunk: args.Chunk,
		})
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				IsError: true,
			}, downloadChunk{}, nil
		}
		meta, err := json.Marshal(chunk)
		if err != nil {
			return nil, downloadChunk{}, err
		}
		// Claude Code renders an image/* blob as a re-encoded JPEG preview
		// rather than saving the original bytes, so images go out as opaque
		// binary; the real type is in the metadata above.
		blobMIME := chunk.MIMEType
		if strings.HasPrefix(blobMIME, "image/") {
			blobMIME = "application/octet-stream"
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: string(meta)},
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
					URI:      fmt.Sprintf("lark-file://%s/%s/%d/%s", args.MessageID, args.FileKey, chunk.Chunk, chunk.FileName),
					MIMEType: blobMIME,
					Blob:     chunk.Data,
				}},
			},
		}, chunk, nil
	}
}
