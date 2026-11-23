// Package filecontent reads the bytes of a file a tool call sends, for the
// tools that put a file into a product: content_base64, the bytes the model
// writes into the call, optionally with their sha256; or, for ChatGPT alone,
// file, the file object ChatGPT passes for a file the person attached in the
// chat, whose bytes the server fetches from OpenAI's file host.
//
// Extracted from basecamp-mcp-server's internal/tools/file_content.go
// (basecamp-mcp-server#422, #423). What each product does with the bytes,
// and which tools take them, stays in the product; so do the filename and
// content type a product's upload asks for beside them.
package filecontent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// Param is the argument carrying ChatGPT's file object, listed in the
	// tool's _meta["openai/fileParams"].
	Param = "file"
	// MetaKey is the _meta key that lists a tool's file arguments for
	// ChatGPT.
	MetaKey = "openai/fileParams"
)

// Meta returns meta with the file argument listed under MetaKey, allocating
// it when nil.
func Meta(meta mcp.Meta) mcp.Meta {
	if meta == nil {
		meta = mcp.Meta{}
	}
	meta[MetaKey] = []string{Param}
	return meta
}

// FileSchema is ChatGPT's file object, exactly the four properties OpenAI's
// reference requires: download_url and file_id required, mime_type and
// file_name declared but optional. Scan Tools and plugin submission reject
// any other shape
// (https://developers.openai.com/plugins/reference, "Define file inputs").
func FileSchema(description string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": description,
		"properties": map[string]any{
			"download_url": map[string]any{"type": "string"},
			"file_id":      map[string]any{"type": "string"},
			"mime_type":    map[string]any{"type": "string"},
			"file_name":    map[string]any{"type": "string"},
		},
		"required":             []string{"download_url", "file_id"},
		"additionalProperties": false,
	}
}

// Properties adds the content arguments every file tool takes to props:
// content_base64, its sha256, and ChatGPT's file object. what names the file
// in the descriptions ("file", "image").
func Properties(props map[string]any, what string) map[string]any {
	props["content_base64"] = map[string]any{
		"type":        "string",
		"description": fmt.Sprintf("The %s content encoded as base64", what),
	}
	props["sha256"] = map[string]any{
		"type":        "string",
		"description": "Hex SHA-256 of the decoded content; a mismatch uploads nothing",
	}
	props[Param] = FileSchema(fmt.Sprintf("The %s attached in this chat, instead of content_base64", what))
	return props
}

// Kind classifies a refusal, so a product answers it in its own words.
type Kind int

const (
	// Invalid is an argument the caller can correct (a product's "invalid
	// arguments:" refusal).
	Invalid Kind = iota
	// Missing is a required argument left out; Error.Param names it.
	Missing
	// TooLarge is content past the size limit.
	TooLarge
	// Malformed is content_base64 that isn't base64.
	Malformed
	// Busy is a fetch refused while other calls hold the budget, the
	// caller's to retry. Its text opens "Rate limited".
	Busy
)

// Error is a refusal of a call's file content. Nothing has been sent
// anywhere when it is returned, and its text never carries a download_url.
type Error struct {
	Kind Kind
	// Param is the missing argument, for Missing.
	Param string
	// Message is the refusal in the caller's words. For Missing it is empty
	// and Error renders the parameter.
	Message string
}

func (e *Error) Error() string {
	if e.Kind == Missing {
		return "missing required parameter: " + e.Param
	}
	return e.Message
}

func invalid(format string, args ...any) *Error {
	return &Error{Kind: Invalid, Message: fmt.Sprintf(format, args...)}
}

func missing(param string) *Error { return &Error{Kind: Missing, Param: param} }

// Options are what a product supplies to Read.
type Options struct {
	// MaxSize is the largest file read, in bytes.
	MaxSize int64
	// RequireBytes refuses empty content.
	RequireBytes bool
	// AcceptsFile reports that the calling client is ChatGPT, the one host
	// that fills a tool's file arguments. Every other client's call naming
	// file is refused as an argument the tool doesn't take, so the server's
	// fetch of a download_url is offered only where ChatGPT supplies it.
	AcceptsFile bool
	// Fetcher reads a file object's download_url; nil is OpenAIFetcher().
	Fetcher *Fetcher
	// Budget bounds the fetched bytes held at once across the process. A
	// fetch with none is refused.
	Budget Budget
	// Logger receives the refusals worth an operator's eye, by host or
	// reason, never a URL; nil discards them.
	Logger *slog.Logger
}

// Content is the file's bytes a call sent.
type Content struct {
	Data []byte
	// release returns what a fetched file holds of the budget.
	release func()
}

// Done releases what the content holds of the budget, if anything. The
// caller defers it, so a fetched file's bytes stay counted through the
// upload that reads them.
func (c *Content) Done() {
	if c != nil && c.release != nil {
		c.release()
	}
}

// Read reads the file's bytes from args, from exactly one of content_base64
// or file, refusing them before anything is sent anywhere when they are
// larger than opts.MaxSize, when sha256 is given and doesn't match them, or,
// with opts.RequireBytes, when there are none. The error, when non-nil, is
// an *Error.
func Read(ctx context.Context, args map[string]any, opts Options) (*Content, error) {
	// Presence is the key's with a non-null value: null reads as absent, as
	// a model may send for an argument it isn't using, and a value of the
	// wrong type is refused, never read as absent.
	hasFile := args[Param] != nil
	rawBase64 := args["content_base64"]
	hasBase64 := rawBase64 != nil
	contentBase64, isString := rawBase64.(string)
	if hasBase64 && !isString {
		return nil, invalid("content_base64 must be a string of base64")
	}
	switch {
	case hasFile && !opts.AcceptsFile:
		return nil, invalid("unknown argument %s: send the content as content_base64", Param)
	case hasFile && hasBase64:
		return nil, invalid("send exactly one of file or content_base64, not both")
	case !hasFile && !hasBase64 && opts.AcceptsFile:
		return nil, invalid("send exactly one of file or content_base64")
	case !hasFile && !hasBase64:
		return nil, missing("content_base64")
	}

	// Refuse a malformed sha256 before reading or fetching anything.
	want, hasSum, err := readSHA256(args)
	if err != nil {
		return nil, err
	}

	var data []byte
	release := func() {}
	if hasFile {
		data, release, err = opts.fetcher().fetch(ctx, args[Param], opts)
	} else {
		data, err = decodeContentBase64(contentBase64, opts.MaxSize)
	}
	if err != nil {
		return nil, err
	}
	if opts.RequireBytes && len(data) == 0 {
		release()
		return nil, invalid("the content is empty: send the file's bytes")
	}
	if hasSum {
		got := sha256.Sum256(data)
		if subtle.ConstantTimeCompare(got[:], want) != 1 {
			release()
			return nil, invalid("the content was altered in transit: its SHA-256 doesn't match sha256, so nothing was uploaded. Re-encode the file from its source and send it again")
		}
	}
	return &Content{Data: data, release: release}, nil
}

func (o Options) fetcher() *Fetcher {
	if o.Fetcher != nil {
		return o.Fetcher
	}
	return OpenAIFetcher()
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// readSHA256 reads the optional sha256 argument: 64 hex digits, either case.
func readSHA256(args map[string]any) (sum []byte, ok bool, err error) {
	value := args["sha256"]
	if value == nil {
		return nil, false, nil
	}
	raw, isString := value.(string)
	if !isString {
		return nil, false, invalid("sha256 must be the content's SHA-256 as 64 hex digits")
	}
	sum, decodeErr := hex.DecodeString(strings.TrimSpace(raw))
	if decodeErr != nil || len(sum) != sha256.Size {
		return nil, false, invalid("sha256 must be the content's SHA-256 as 64 hex digits")
	}
	return sum, true, nil
}

// decodeContentBase64 decodes content_base64, refusing it before and after
// decoding when it is larger than maxSize.
func decodeContentBase64(contentBase64 string, maxSize int64) ([]byte, error) {
	// Clamp to math.MaxInt to avoid int64→int overflow on EncodedLen.
	n := min(maxSize, math.MaxInt/2)
	maxBase64Size := base64.StdEncoding.EncodedLen(int(n))
	if len(contentBase64) > maxBase64Size {
		return nil, &Error{Kind: TooLarge, Message: fmt.Sprintf("file too large: base64 content exceeds %d bytes (max decoded size: %d bytes)", maxBase64Size, maxSize)}
	}
	data, err := base64.StdEncoding.DecodeString(contentBase64)
	if err != nil {
		return nil, &Error{Kind: Malformed, Message: fmt.Sprintf("invalid base64 encoding in content_base64: %v", err)}
	}
	if int64(len(data)) > maxSize {
		return nil, &Error{Kind: TooLarge, Message: fmt.Sprintf("file too large: %d bytes exceeds maximum of %d bytes", len(data), maxSize)}
	}
	return data, nil
}
