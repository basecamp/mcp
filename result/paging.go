package result

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Paging what the cap cuts.
//
// The cap cuts a listing to whole items and says where to continue only when
// the listing takes an offset, and it has nothing to cut in a single record
// but a flagged prefix of its JSON: the body cut mid-string, every field
// after it lost, nothing to continue from. basecamp-mcp-server fixed both in
// its handlers (#307 for records, #309 for listings); a server built on this
// toolkit gets the same dialect from the middleware instead, by naming its
// tools in Paging, with no change to the handlers behind them:
//
//   - A record read (Paging.Records) takes content_offset. A record too
//     large to return whole has its rich-text fields (Paging.TextFields) served
//     as a window of bytes from content_offset, cut on rune boundaries, every
//     other field whole. The record carries "truncated": true and a
//     "truncation" with fields (each field's full length, by its path),
//     offset, returned_bytes and, while more remains,
//     continue.content_offset. One offset pages every field together, so a
//     record that carries its body twice (text and HTML) stays in step.
//     These are #307's names, so every server speaks one dialect.
//   - A page-based listing (Paging.Listings) takes offset: the items to skip
//     at the start of the page its page argument names. A cut listing names
//     page (as the caller passed it, a number or an opaque cursor) and
//     offset, so truncation.continue resumes at the first item cut, and its
//     next_page, kept, comes after. An item too large alone is named by its
//     app_url, where it has one, and stepped past.
//
// The middleware owns both arguments: it adds them to the named tools'
// input schemas on tools/list, and on tools/call takes them out of the
// arguments before the handler sees them, so a handler (or a gateway that
// sends every argument upstream) never sees an argument it doesn't know. A
// tool named here must not take an argument of the same name itself; one
// whose handler already pages by offset (basecamp-mcp-server's pageResult,
// fizzy-mcp-server's flat listings) is not named, and the cap continues it
// as it always has.

// ContentOffsetKey is the argument, and the continuation key, that pages a
// record's rich text; OffsetKey the one that continues a page of a listing;
// PageKey the argument a page-based listing takes its page by.
const (
	ContentOffsetKey = "content_offset"
	OffsetKey        = "offset"
	PageKey          = "page"
)

// DefaultTextFields are the rich-text fields a record read pages when
// Paging.TextFields is empty: the names Basecamp, HEY and Fizzy give a
// record's body, matched at any depth of nested objects (Fizzy's comment
// body.html and body.plain_text), never inside a list.
var DefaultTextFields = []string{
	"content", "content_html", "description", "description_html",
	"body", "html", "plain_text", "note", "note_html",
}

// contentOffsetDescription and offsetDescription are the schema
// descriptions the middleware gives the arguments it adds.
const (
	contentOffsetDescription = "Byte offset into the record's rich text (its body, content or description) to read from (default 0). " +
		"A record too large to return whole is cut, and its truncation.continue gives the content_offset that reads on."
	offsetDescription = "Items to skip at the start of the page (default 0); a truncated result names the offset that continues it."
)

// contentHint and contentEndHint explain a record cut by capRecord.
const (
	contentHint    = "The record's rich text exceeded the server's size limit and was cut to fit; every other field is whole. Call again with truncation.continue.content_offset as content_offset to read the next part."
	contentEndHint = "This is the last part of the record's rich text, read from truncation.offset; every other field is whole."
)

// Paging names the tools whose results the middleware pages. A nil
// predicate names none.
type Paging struct {
	// Records names the tools that read one record: they take
	// content_offset, and an oversized record's rich text is paged.
	Records func(tool string) bool
	// Listings names the page-based listings whose handlers take no offset:
	// they take offset, and a cut one continues within its page.
	Listings func(tool string) bool
	// TextFields are the rich-text field names a record read pages;
	// DefaultTextFields when empty.
	TextFields []string
}

// pagingKind is what the middleware does for one tool.
type pagingKind int

const (
	pagesNothing pagingKind = iota
	pagesRecord
	pagesListing
)

func (p Paging) kind(tool string) pagingKind {
	switch {
	case p.Records != nil && p.Records(tool):
		return pagesRecord
	case p.Listings != nil && p.Listings(tool):
		return pagesListing
	}
	return pagesNothing
}

func (p Paging) textFields() []string {
	if len(p.TextFields) == 0 {
		return DefaultTextFields
	}
	return p.TextFields
}

// argument names the argument the middleware owns for a kind, and its
// schema description.
func (k pagingKind) argument() (string, string) {
	switch k {
	case pagesRecord:
		return ContentOffsetKey, contentOffsetDescription
	case pagesListing:
		return OffsetKey, offsetDescription
	}
	return "", ""
}

// callTool runs a tools/call for a paged tool: the argument the middleware
// owns is read and taken out before the handler runs, and the result paged
// by it after.
func (p Paging) callTool(ctx context.Context, next mcp.MethodHandler, req *mcp.CallToolRequest, maxBytes int) (mcp.Result, error) {
	kind := pagingKind(pagesNothing)
	if req != nil && req.Params != nil {
		kind = p.kind(req.Params.Name)
	}
	if kind == pagesNothing {
		res, err := next(ctx, "tools/call", req)
		if r, ok := res.(*mcp.CallToolResult); ok && err == nil {
			return Cap(Object(r), maxBytes), nil
		}
		return res, err
	}
	key, _ := kind.argument()
	args, offset, page, errResult := takeArgument(req.Params.Arguments, key)
	if errResult != nil {
		return errResult, nil
	}
	stripped := *req
	params := *req.Params
	params.Arguments = args
	stripped.Params = &params
	res, err := next(ctx, "tools/call", &stripped)
	r, ok := res.(*mcp.CallToolResult)
	if err != nil || !ok {
		return res, err
	}
	r = Object(r)
	if kind == pagesRecord {
		return capRecord(r, maxBytes, offset, p.textFields()), nil
	}
	return capListing(r, maxBytes, offset, page), nil
}

// takeArgument reads key from a call's raw arguments as an integer >= 0
// (absent is 0) and returns the arguments without it, and the page argument
// as passed (nil when absent). Arguments without key pass through as they
// came.
func takeArgument(raw json.RawMessage, key string) (json.RawMessage, int, any, *mcp.CallToolResult) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, 0, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil || args == nil {
		// Not an object: the handler's to refuse.
		return raw, 0, nil, nil
	}
	page := args[PageKey]
	value, present := args[key]
	if !present {
		return raw, 0, page, nil
	}
	n, ok := nonNegativeInt(value)
	if !ok {
		return nil, 0, nil, errorResult("%s must be an integer >= 0", key)
	}
	delete(args, key)
	data, err := json.Marshal(args)
	if err != nil {
		return nil, 0, nil, errorResult("invalid arguments")
	}
	return data, n, page, nil
}

// nonNegativeInt reads a JSON number that is a whole number >= 0 (null is
// 0, as if absent).
func nonNegativeInt(v any) (int, bool) {
	if v == nil {
		return 0, true
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := num.Int64(); err == nil {
		return int(i), i >= 0 && i <= math.MaxInt32
	}
	f, err := num.Float64()
	if err != nil || f != math.Trunc(f) || f < 0 || f > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}

// errorResult is a tool error the middleware answers with.
func errorResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

// advertise adds the argument the middleware owns to each paged tool's
// input schema, unless the schema already declares it. Tools are copied,
// never written through.
func (p Paging) advertise(result *mcp.ListToolsResult) *mcp.ListToolsResult {
	if result == nil || (p.Records == nil && p.Listings == nil) {
		return result
	}
	out := *result
	out.Tools = make([]*mcp.Tool, len(result.Tools))
	for i, tool := range result.Tools {
		out.Tools[i] = tool
		if tool == nil {
			continue
		}
		key, description := p.kind(tool.Name).argument()
		if key == "" {
			continue
		}
		if schema, ok := withProperty(tool.InputSchema, key, description); ok {
			t := *tool
			t.InputSchema = schema
			out.Tools[i] = &t
		}
	}
	return &out
}

// withProperty returns a copy of an object input schema with an integer
// property key added; false when the schema isn't an object schema or
// already declares key.
func withProperty(schema any, key, description string) (map[string]any, bool) {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return nil, false
	}
	if t, _ := m["type"].(string); t != "object" {
		return nil, false
	}
	props, _ := m["properties"].(map[string]any)
	if _, declared := props[key]; declared {
		return nil, false
	}
	props = maps.Clone(props)
	if props == nil {
		props = map[string]any{}
	}
	props[key] = map[string]any{"type": "integer", "minimum": 0, "description": description}
	m["properties"] = props
	return m, true
}

// contentContinuation is where a cut record's rich text resumes.
type contentContinuation struct {
	ContentOffset int `json:"content_offset"`
}

// contentTruncation explains a record whose rich text was served as a
// window: #307's truncation, field for field.
type contentTruncation struct {
	LimitBytes    int `json:"limit_bytes,omitempty"`
	OriginalBytes int `json:"original_bytes"`
	// Fields are the rich-text fields cut, each with its full length in
	// bytes, by path ("body.html" for a field nested in an object).
	Fields map[string]int `json:"fields"`
	// Offset is where the window served starts; ReturnedBytes is its length.
	Offset        int                  `json:"offset"`
	ReturnedBytes int                  `json:"returned_bytes"`
	Continue      *contentContinuation `json:"continue,omitempty"`
	Hint          string               `json:"hint"`
}

// capRecord holds a record read's result to maxBytes, paging its rich text
// from offset. A record that fits is served as Cap serves it when offset is
// 0. Otherwise its rich-text fields are cut to the window from offset that
// fits, every other field whole; a record that can't fit even with its rich
// text emptied goes to Cap. An offset on a result with no rich text, or past
// its end, is refused.
func capRecord(result *mcp.CallToolResult, maxBytes, offset int, fields []string) *mcp.CallToolResult {
	if result == nil || result.IsError || asksForInput(result) {
		return result
	}
	tc, value, restated, ok := standard(result)
	record, isObject := value.(map[string]any)
	if !ok || !isObject {
		if offset > 0 {
			return errorResult("%s applies only to a record; this result has no rich text to page", ContentOffsetKey)
		}
		return Cap(result, maxBytes)
	}
	texts := textFields(record, fields)
	longest := 0
	for _, f := range texts {
		longest = max(longest, len(f.text))
	}
	if offset > 0 && len(texts) == 0 {
		return errorResult("%s applies only to a record with rich text; this one has none", ContentOffsetKey)
	}
	if offset > 0 && offset >= longest {
		return errorResult("%s %d is past the end of the record's rich text (%d bytes)", ContentOffsetKey, offset, longest)
	}
	limit := math.MaxInt
	if maxBytes > 0 {
		limit = max(maxBytes, MinBytes)
	}
	size, err := wireBytes(result)
	if offset == 0 && (len(texts) == 0 || (err == nil && size <= limit-frameReserve)) {
		return Cap(result, maxBytes)
	}

	serve := func(payload []byte) *mcp.CallToolResult {
		out := *result
		out.Content = []mcp.Content{withText(tc, string(payload))}
		if restated {
			out.StructuredContent = json.RawMessage(payload)
		}
		return &out
	}
	fits := func(payload []byte) bool {
		if payload == nil {
			return false
		}
		n, err := wireBytes(serve(payload))
		return err == nil && n <= limit-frameReserve
	}
	if offset == 0 {
		if compact, err := json.Marshal(record); err == nil && fits(compact) {
			return serve(compact)
		}
	}
	lengths := make(map[string]int, len(texts))
	for _, f := range texts {
		lengths[f.name] = len(f.text)
	}
	reported := limit
	if maxBytes <= 0 {
		reported = 0
	}
	// render serves the window [offset, offset+n) of each rich-text field,
	// cut on rune boundaries; it reports how far past offset the longest
	// window reaches, which is where the next window starts.
	render := func(n int) ([]byte, int) {
		out := cloneObjects(record)
		served := 0
		for _, f := range texts {
			window, end := textWindow(f.text, offset, n)
			setPath(out, f.path, window)
			served = max(served, end-offset)
		}
		t := contentTruncation{
			LimitBytes: reported, OriginalBytes: size, Fields: lengths,
			Offset: offset, ReturnedBytes: served, Hint: contentEndHint,
		}
		if next := offset + served; next < longest {
			t.Continue = &contentContinuation{ContentOffset: next}
			t.Hint = contentHint
		}
		out["truncated"] = true
		out["truncation"] = t
		data, err := json.Marshal(out)
		if err != nil {
			return nil, 0
		}
		return data, served
	}
	n := FitLeading(longest-offset, func(n int) bool {
		data, _ := render(n)
		return fits(data)
	})
	data, served := render(n)
	if served <= 0 || !fits(data) {
		// Not even one character of rich text fits beside the record's
		// other fields, so a window could only offer a continuation back to
		// the same offset. The record goes to the cap's own cut.
		return Cap(result, maxBytes)
	}
	return serve(data)
}

// textField is a rich-text field of a record: its path through nested
// objects, the name truncation.fields gives it, and its text.
type textField struct {
	path []string
	name string
	text string
}

// textFields returns the non-empty string fields of record named in names,
// at any depth of nested objects; lists are not entered.
func textFields(record map[string]any, names []string) []textField {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	var out []textField
	var walk func(m map[string]any, path []string)
	walk = func(m map[string]any, path []string) {
		for k, v := range m {
			at := append(append([]string(nil), path...), k)
			switch v := v.(type) {
			case string:
				if wanted[k] && v != "" {
					out = append(out, textField{path: at, name: strings.Join(at, "."), text: v})
				}
			case map[string]any:
				walk(v, at)
			}
		}
	}
	walk(record, nil)
	return out
}

// cloneObjects copies m and every object nested in it (not lists, which
// paging never enters), so a window can be set at any path without writing
// through.
func cloneObjects(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		if nested, ok := v.(map[string]any); ok {
			v = cloneObjects(nested)
		}
		out[k] = v
	}
	return out
}

// setPath sets the value at a path textFields returned, in a clone.
func setPath(m map[string]any, path []string, value string) {
	for _, k := range path[:len(path)-1] {
		m = m[k].(map[string]any)
	}
	m[path[len(path)-1]] = value
}

// textWindow returns the part of s from offset up to offset+n bytes, without
// splitting a rune at either end, and the byte position it ends at. An offset
// inside a rune moves back to the rune's start, so a window never loses a
// character; it may repeat the bytes of one.
func textWindow(s string, offset, n int) (string, int) {
	if offset >= len(s) {
		return "", offset
	}
	start := offset
	for start > 0 && !utf8.RuneStart(s[start]) {
		start--
	}
	window := utf8Prefix(s[start:], n+offset-start)
	if len(window) <= offset-start {
		return "", offset
	}
	return window, start + len(window)
}

// capListing holds a page-based listing's result to maxBytes, serving its
// one list from offset: the items before it are dropped, and the payload
// names offset and the page it was served at (the page/offset contract the
// cap continues exactly). A listing that fits at offset 0 is served as Cap
// serves it, unnamed. An offset on a result with no one top-level list is
// refused, as is one on a listing whose handler names its own offset (it was
// never handed the argument).
func capListing(result *mcp.CallToolResult, maxBytes, offset int, page any) *mcp.CallToolResult {
	if result == nil || result.IsError || asksForInput(result) {
		return result
	}
	tc, value, restated, ok := standard(result)
	payload, isObject := value.(map[string]any)
	field := ""
	if ok && isObject && arrayFields(payload) == 1 {
		for k, v := range payload {
			if _, isList := v.([]any); isList {
				field = k
			}
		}
	}
	_, ownsOffset := payload[OffsetKey]
	if field == "" || ownsOffset {
		if offset > 0 {
			return errorResult("%s applies only to a listing with one list of items", OffsetKey)
		}
		return Cap(result, maxBytes)
	}
	if offset == 0 && maxBytes > 0 {
		if size, err := wireBytes(result); err == nil && size <= max(maxBytes, MinBytes)-frameReserve {
			return result
		}
	}
	if offset == 0 && maxBytes <= 0 {
		return result
	}
	out := maps.Clone(payload)
	items := payload[field].([]any)
	out[field] = items[min(offset, len(items)):]
	out[OffsetKey] = offset
	if page != nil && page != "" {
		out[PageKey] = page
	}
	data, err := Marshal(out, true)
	if err != nil {
		return Cap(result, maxBytes)
	}
	served := *result
	served.Content = []mcp.Content{withText(tc, string(data))}
	if restated {
		served.StructuredContent = json.RawMessage(data)
	}
	return Cap(&served, maxBytes)
}
