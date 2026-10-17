package result

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"sort"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool-result size cap.
//
// Every tool result passes through Cap on its way out of the server (a
// product installs Middleware, or calls Cap where it serves results). The
// cap measures what goes on the wire: the result as the SDK serializes it,
// JSON string escaping, the content envelope and _meta included, plus
// frameReserve for what is added after it (see there). A result that fits is
// untouched. One that does not is cut to fit here, where the cut can be
// explained, rather than by the host, which truncates or refuses an
// oversized result on its own terms and tells the model nothing about what
// is missing.
//
// There are two cuts, by shape.
//
// The exact cut is for the shape every server serves: one text block holding
// a JSON document, which structuredContent, when present, restates (what
// JSON builds). Its JSON is first re-rendered compact (the indentation JSON
// adds is a third of a typical payload); if that fits, nothing is lost and
// nothing is flagged. Otherwise a list is shortened to as many leading items
// as fit — but only where shortening cannot corrupt a continuation: a bare
// list (a top-level array, or the {"items": [...]} object JSON serves one
// as; nothing beside it can point past the cut), or an object that declares
// the offset contract below. The payload gains "truncated": true and a
// "truncation" object saying which list was cut, how many items it kept of
// how many, and how to see the rest. Any other object — the event feed's
// position and next, a cursor page's next_page, anything carrying a
// continuation this layer cannot rewrite — keeps a flagged prefix of its
// compact text as "partial_json" instead: a shortened list next to an
// untouched cursor would let a caller resume past the items cut, silently.
// Every candidate cut is sized by serializing the whole result it would
// produce, so escaping can't push a cut over the limit.
//
// A prefix is read by a model, so a continuation must not survive in it
// either. The prefix renders every object's arrays and objects first,
// largest first, and its scalars after them, at every depth: a cursor, a
// position or a next URL is a scalar, so it shows only when every list
// before it in its object is whole — when it cannot point past anything cut.
// (A continuation held in an object larger than the list it continues, and
// so rendered before it, is not covered by that order; no payload here has
// one.)
//
// A listing that takes an offset names the offset it was served at, and its
// page when it has one, as top-level "offset" and "page" beside its one list
// (the page/offset contract; see pageContinuationOf); cutting its list adds
// truncation.continue, the page and offset of the first item cut, because
// those listings have no page size to shrink — re-requesting would cut the
// same way, and following next_page would skip the cut items. An item too
// large to be served even alone is named in truncation.oversized_item (by
// position, and id when it has one) and the continuation steps past it, so
// continuing always makes progress.
//
// Every other shape — several content blocks, prose beside structured
// content, structured content alone, plain text, non-text content — gets the
// coarse cut: the result is replaced by one text notice saying it was
// withheld, how large it was and what the limit is, with structuredContent
// reduced to the same explanation when the result had any. Its size depends
// only on two integers, so it fits under MinBytes, frameReserve included,
// whatever the result held.
// No current server returns such a result over the cap; one that needs a
// finer cut for another shape brings that case here.
//
// A listing whose continuation this layer cannot rewrite but its handler can
// (basecamp-mcp-server's list_projects and its signed next_cursor) cuts
// itself before it gets here, with FitsWhole and FitLeading: whole items, and
// a continuation minted at the first item cut.

// truncationHint tells the caller how to get what was cut when the result
// carries no exact continuation. It names no argument: whether a limit, page
// or filter reaches past the cut is the tool's own paging logic, which an
// argument name cannot show (a smaller limit returns a shorter prefix of the
// same items; some listings declare page and ignore it). An exact
// continuation comes only from that logic, as truncation.continue (see
// continueHint) or a tool's own cut envelope.
const truncationHint = "The result exceeded the server's size limit and was cut, keeping what came first. To get the rest, narrow the request with filters, or read a specific record directly."

// continueHint replaces truncationHint when the cut list is a page of a
// page-numbered listing, which can be continued exactly.
const continueHint = "The result exceeded the server's size limit and was cut to fit. Call again with the page and offset in truncation.continue to get the items that were cut, before following next_page."

// offsetHint replaces truncationHint when the cut list belongs to a listing
// that takes only an offset: there is no page and no next_page to follow.
const offsetHint = "The result exceeded the server's size limit and was cut to fit. Call again with the offset in truncation.continue to get the items that were cut."

// oversizedHint replaces the hint when the next item exceeds the limit alone.
const oversizedHint = "The next item exceeds the server's size limit on its own and cannot be returned; truncation.oversized_item names it by position and id. truncation.continue, when present, resumes after it."

// withheldHint explains the coarse cut.
const withheldHint = "The result exceeded the server's size limit and could not be cut to fit, so it was withheld. Narrow the request with filters, or read a specific record directly."

// frameReserve is the room the cap leaves for what is added to a result
// after Cap sees it: the SDK's resultType and its serverInfo _meta entry
// (both under protocol 2026-07-28, about 120 bytes for a server identified by
// name and version), and the JSON-RPC frame around the result (about 40).
// Holding the result to the limit less this keeps the message under the
// limit. A server whose Implementation serializes to more than a few hundred
// bytes (icons, say) overruns it by the difference.
const frameReserve = 512

// MinBytes is the smallest cap enforced: room for the truncation envelope and
// a useful amount of payload, and always for the coarse cut. A smaller
// positive setting is raised to it.
const MinBytes = 4096

// pageContinuation is where a cut listing resumes. Page is absent for the
// unpaginated listings that take only an offset.
type pageContinuation struct {
	Page   *int64 `json:"page,omitempty"`
	Offset int64  `json:"offset"`
}

// oversizedItem names an item that cannot fit within the limit on its own.
type oversizedItem struct {
	Index int `json:"index"`
	ID    any `json:"id,omitempty"`
}

// truncation is the explanation attached to a cut result.
type truncation struct {
	LimitBytes    int               `json:"limit_bytes"`
	OriginalBytes int               `json:"original_bytes"`
	Field         string            `json:"field,omitempty"`
	Returned      *int              `json:"returned,omitempty"`
	Total         *int              `json:"total,omitempty"`
	Continue      *pageContinuation `json:"continue,omitempty"`
	OversizedItem *oversizedItem    `json:"oversized_item,omitempty"`
	Hint          string            `json:"hint"`
}

// Cap holds a successful result to maxBytes of serialized result, frameReserve
// included. It returns
// the result unchanged when maxBytes <= 0, the result is an error (an error
// must reach the caller whole), or it already fits; a positive maxBytes below
// MinBytes is raised to it. The result passed in is never modified.
//
// The one-text-block JSON shape gets the exact cut; any other shape the
// coarse one (see the package's size-cap notes).
func Cap(result *mcp.CallToolResult, maxBytes int) *mcp.CallToolResult {
	if result == nil || result.IsError || maxBytes <= 0 {
		return result
	}
	limit := max(maxBytes, MinBytes)
	size, err := wireBytes(result)
	if err == nil && size <= limit-frameReserve {
		return result
	}
	if out := capExact(result, size, limit); out != nil {
		return out
	}
	return capCoarse(result, size, limit)
}

// wireBytes is the size of result as the SDK serializes it.
func wireBytes(result *mcp.CallToolResult) (int, error) {
	data, err := json.Marshal(result)
	return len(data), err
}

// standard returns, for a result in the shape the exact cut handles — one
// text block holding a JSON document, restated by structuredContent or
// standing alone — the block, its parsed value and whether structured content
// restates it.
func standard(result *mcp.CallToolResult) (tc *mcp.TextContent, value any, restated, ok bool) {
	if len(result.Content) != 1 {
		return nil, nil, false, false
	}
	tc, isText := result.Content[0].(*mcp.TextContent)
	if !isText {
		return nil, nil, false, false
	}
	value, isJSON := decodeJSON([]byte(tc.Text))
	if !isJSON {
		return nil, nil, false, false
	}
	if result.StructuredContent == nil {
		return tc, value, false, true
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil || !sameJSON([]byte(tc.Text), structured) {
		return nil, nil, false, false
	}
	return tc, value, true, true
}

// capExact cuts a result in the standard shape, or returns nil when it is in
// another shape or no cut of its JSON fits (its _meta alone too large, say).
func capExact(result *mcp.CallToolResult, originalBytes, limit int) *mcp.CallToolResult {
	tc, value, restated, ok := standard(result)
	if !ok {
		return nil
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
		n, err := wireBytes(serve(payload))
		return err == nil && n <= limit-frameReserve
	}
	payload := capJSON(value, originalBytes, limit, fits)
	if payload == nil {
		return nil
	}
	return serve(payload)
}

// capCoarse replaces a result the exact cut can't serve with one notice,
// and its structured content, if any, with the same explanation. The result's
// _meta is kept when it fits beside the notice and dropped otherwise, so the
// cut depends only on originalBytes and limit, and fits under MinBytes less
// frameReserve.
func capCoarse(result *mcp.CallToolResult, originalBytes, limit int) *mcp.CallToolResult {
	t := truncation{LimitBytes: limit, OriginalBytes: originalBytes, Hint: withheldHint}
	out := &mcp.CallToolResult{
		Meta: result.Meta,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
			"[truncated: this %d-byte result exceeded the server's %d-byte limit and was withheld. %s]",
			originalBytes, limit, withheldHint)}},
	}
	if result.StructuredContent != nil {
		out.StructuredContent = map[string]any{"truncated": true, "truncation": t}
	}
	if n, err := wireBytes(out); err != nil || n > limit-frameReserve {
		out.Meta = nil
	}
	return out
}

// decodeJSON parses data as exactly one JSON document, numbers kept
// exact. Trailing bytes, even a stray closing bracket, make it not JSON.
func decodeJSON(data []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// withText copies a text block with new text, keeping its _meta and
// annotations.
func withText(tc *mcp.TextContent, text string) *mcp.TextContent {
	out := *tc
	out.Text = text
	return &out
}

// capJSON renders value as the payload of a result that fits: compact if
// that fits, else with its largest list shortened, else as a flagged prefix
// of its compact text; nil when not even an empty prefix fits.
func capJSON(value any, originalBytes, limit int, fits func([]byte) bool) []byte {
	compact, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	if fits(compact) {
		return compact
	}
	if out := shortenLargestList(value, originalBytes, limit, fits); out != nil {
		return out
	}
	if ordered, err := prefixOrder(value); err == nil {
		compact = ordered
	}
	return prefixJSON(string(compact), originalBytes, limit, fits)
}

// prefixOrder renders a value compactly for the prefix fallback, every
// object in it — nested ones and list items included — with its arrays and
// objects first, largest first, then its scalars, smallest first. A prefix
// ends inside whatever it cuts, so a scalar in it — a cursor, a position, a
// next URL, at any depth — is there only if every array and object before it
// in its own object is there whole, and a continuation beside whole lists
// skips nothing. Equal sizes keep name order, so the rendering is
// deterministic.
func prefixOrder(v any) ([]byte, error) {
	switch v := v.(type) {
	case map[string]any:
		type field struct {
			key, value []byte
			structured bool
		}
		fields := make([]field, 0, len(v))
		for k, val := range v {
			key, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			value, err := prefixOrder(val)
			if err != nil {
				return nil, err
			}
			_, isList := val.([]any)
			_, isObject := val.(map[string]any)
			fields = append(fields, field{key, value, isList || isObject})
		}
		sort.Slice(fields, func(i, j int) bool {
			a, b := fields[i], fields[j]
			switch {
			case a.structured != b.structured:
				return a.structured
			case len(a.value) != len(b.value):
				return (len(a.value) > len(b.value)) == a.structured
			default:
				return bytes.Compare(a.key, b.key) < 0
			}
		})
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, f := range fields {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(f.key)
			buf.WriteByte(':')
			buf.Write(f.value)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			data, err := prefixOrder(item)
			if err != nil {
				return nil, err
			}
			buf.Write(data)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	default:
		return json.Marshal(v)
	}
}

// FitsWhole reports whether v, served as JSON(v), passes Cap without a cut:
// whether its compact rendering, text and structured copy, fits on the wire.
// maxBytes <= 0 means no cap. A handler that owns a continuation this layer
// cannot rewrite sizes its own page with it (see FitLeading).
func FitsWhole(v any, maxBytes int) bool {
	if maxBytes <= 0 {
		return true
	}
	data, err := Marshal(v, false)
	if err != nil {
		return false
	}
	n, err := wireBytes(&mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
		StructuredContent: json.RawMessage(data),
	})
	return err == nil && n <= max(maxBytes, MinBytes)-frameReserve
}

// JSONBytes is what Cap measures for JSON(v): the serialized result, its
// indented text and structured copy together. A handler that cuts its own
// page reports it as the original size.
func JSONBytes(v any) int {
	n, err := wireBytes(JSON(v))
	if err != nil {
		return 0
	}
	return n
}

// FitLeading returns the largest k in [0, n] for which fits(k) holds, given
// that fewer items never render larger; 0 when none does (fits(0) is then
// unverified, and the caller decides what an empty page means).
func FitLeading(n int, fits func(k int) bool) int {
	if n <= 0 || fits(n) {
		return max(n, 0)
	}
	lo, hi := 0, n-1
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// shortenLargestList cuts the payload's largest list to the most leading
// items that fit, or returns nil when it can't be cut safely: no list, a
// continuation that could belong to another list, or no cut that fits and
// makes progress.
func shortenLargestList(value any, originalBytes, limit int, fits func([]byte) bool) []byte {
	var (
		items []any
		field string
		build func(kept []any, t truncation) map[string]any
		cont  func(returned int) *pageContinuation
	)
	switch v := value.(type) {
	case []any:
		items, field = v, ItemsKey
		build = func(kept []any, t truncation) map[string]any {
			return map[string]any{ItemsKey: kept, "truncated": true, "truncation": t}
		}
	case map[string]any:
		field = largestArrayField(v)
		arr, ok := v[field].([]any)
		if !ok {
			return nil
		}
		cont = pageContinuationOf(v)
		if cont != nil && arrayFields(v) > 1 {
			// The offset could belong to any of the lists; continuing the
			// largest one could skip items of the list it really pages.
			return nil
		}
		if cont == nil && !bareList(v) {
			// No offset contract: this object may carry a continuation
			// (a cursor, a position) that would point past a cut. Leave it
			// to the prefix path, which keeps no structured continuation.
			return nil
		}
		items = arr
		build = func(kept []any, t truncation) map[string]any {
			out := maps.Clone(v)
			out[field] = kept
			out["truncated"] = true
			out["truncation"] = t
			return out
		}
	default:
		return nil
	}

	total := len(items)
	if total == 0 {
		return nil
	}
	// render keeps the first k items; oversized marks item k as too large to
	// fit alone, so the continuation steps past it.
	render := func(k int, oversized, withID bool) []byte {
		t := truncation{
			LimitBytes: limit, OriginalBytes: originalBytes, Field: field,
			Returned: &k, Total: &total, Hint: truncationHint,
		}
		next := k
		if cont != nil {
			t.Hint = continueHint
			if cont(0).Page == nil {
				t.Hint = offsetHint
			}
		}
		if oversized {
			t.OversizedItem = &oversizedItem{Index: k}
			if withID {
				t.OversizedItem.ID = itemID(items[k])
			}
			t.Hint = oversizedHint
			next = k + 1
		}
		if cont != nil && next < total {
			t.Continue = cont(next)
		}
		data, err := json.Marshal(build(items[:k], t))
		if err != nil {
			return nil
		}
		return data
	}
	fitsRendered := func(data []byte) bool { return data != nil && fits(data) }

	// Never a zero-item page: its continuation would point back at the same
	// offset forever. When not even one item fits, it is skipped as
	// oversized, or the list isn't cut here.
	if k := FitLeading(total, func(k int) bool { return fitsRendered(render(k, false, false)) }); k > 0 {
		return render(k, false, false)
	}
	// The first item doesn't fit beside the envelope. It is oversized only if
	// it can't be served even with nothing beside it but the envelope and the
	// page and offset that place it; if it could, what crowds it out is the
	// rest of the payload, and stepping past it would skip an item that is
	// not too large to read.
	if fitsRendered(minimal(render(1, false, false), field)) {
		return nil
	}
	for _, withID := range []bool{true, false} {
		if out := render(0, true, withID); fitsRendered(out) {
			return out
		}
	}
	// Not even the marker naming the oversized item fits: there is no cut
	// that makes progress, so the prefix fallback, which keeps no
	// continuation that could return to it, takes over.
	return nil
}

// minimal keeps, of a rendered page, only its list, the envelope and the
// page and offset that place it: the least any page holding its items
// carries.
func minimal(page []byte, field string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(page, &m) != nil {
		return nil
	}
	for k := range m {
		switch k {
		case field, "truncated", "truncation", "page", "offset":
		default:
			delete(m, k)
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return data
}

// bareList reports whether m is a list served alone: {"items": [...]} with
// nothing beside it, which is how JSON serves a top-level array. Like a
// bare array, it carries no continuation a cut could leave pointing past the
// items removed.
func bareList(m map[string]any) bool {
	_, isList := m[ItemsKey].([]any)
	return isList && len(m) == 1
}

// arrayFields counts the top-level fields of m that are lists.
func arrayFields(m map[string]any) int {
	n := 0
	for _, v := range m {
		if _, ok := v.([]any); ok {
			n++
		}
	}
	return n
}

// itemID returns an item's "id", if it is an object that has one.
func itemID(item any) any {
	if m, ok := item.(map[string]any); ok {
		return m["id"]
	}
	return nil
}

// pageContinuationOf returns, for the payload of a listing that takes an
// offset (one carrying a numeric "offset", and "page" when paginated), the
// continuation after `next` items; nil for any other payload.
func pageContinuationOf(m map[string]any) func(next int) *pageContinuation {
	offset, ok := jsonInt(m["offset"])
	if !ok {
		return nil
	}
	var page *int64
	if p, isPaged := jsonInt(m["page"]); isPaged {
		page = &p
	}
	return func(next int) *pageContinuation {
		return &pageContinuation{Page: page, Offset: offset + int64(next)}
	}
}

// jsonInt reads an integer decoded with UseNumber.
func jsonInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// largestArrayField names the non-empty array-valued top-level field with
// the largest rendering, or "" when there is none. Ties go to the first name
// in order, so the choice is deterministic.
func largestArrayField(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, bestSize := "", -1
	for _, k := range keys {
		arr, ok := m[k].([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		data, err := json.Marshal(arr)
		if err != nil {
			continue
		}
		if len(data) > bestSize {
			best, bestSize = k, len(data)
		}
	}
	return best
}

// prefixJSON wraps the longest prefix of the compact text that fits in a
// flagged envelope, or returns nil when not even an empty prefix fits. A
// longer prefix never serializes smaller, so the longest is found by
// bisection on the serialized result, escaping included.
func prefixJSON(compact string, originalBytes, limit int, fits func([]byte) bool) []byte {
	t := truncation{LimitBytes: limit, OriginalBytes: originalBytes, Hint: truncationHint}
	render := func(keep int) []byte {
		data, err := json.Marshal(map[string]any{
			"truncated":    true,
			"truncation":   t,
			"partial_json": utf8Prefix(compact, keep),
		})
		if err != nil {
			return nil
		}
		return data
	}
	fitsKeep := func(keep int) bool {
		data := render(keep)
		return data != nil && fits(data)
	}
	keep := FitLeading(len(compact), fitsKeep)
	if keep == 0 && !fitsKeep(0) {
		return nil
	}
	return render(keep)
}

// utf8Prefix returns at most n bytes of s without splitting a rune.
func utf8Prefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
