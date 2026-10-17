package result

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool-result size cap.
//
// Every tool result passes through Cap on its way out of the server (a
// product installs Middleware, or calls Cap where it serves results).
// A result whose text fits the cap is untouched. One that does not is cut to
// fit here, where the cut can be explained, rather than by the host, which
// truncates or refuses an oversized result on its own terms and tells the
// model nothing about what is missing.
//
// A JSON result is first re-rendered compact (the indentation JSON adds
// is a third of a typical payload); if that fits, nothing is lost and nothing
// is flagged. Otherwise a list is shortened to as many leading items as fit —
// but only where shortening cannot corrupt a continuation: a bare list (a
// top-level array, or the {"items": [...]} object JSON serves one as;
// nothing beside it can point past the cut), or an object that declares the
// offset contract below. The payload gains "truncated": true and a
// "truncation" object saying which list was cut, how many items it kept of
// how many, and how to see the rest. Any other object — the event feed's
// position and next, anything carrying a continuation this layer cannot
// rewrite — keeps a flagged prefix of its text instead: a shortened list next
// to an untouched cursor would let a caller resume past the items cut,
// silently. Plain text keeps a prefix too.
//
// A prefix is read by a model, so a continuation must not survive in it
// either. The prefix renders the object's arrays and objects first, largest
// first, and its scalars after them: a cursor, a position or a next URL is a
// scalar, so it shows only when every list before it is whole — when it
// cannot point past anything cut. (A continuation nested in an object smaller
// than the list it continues is not covered by that order; no payload here
// has one.)
//
// A listing whose continuation this layer cannot rewrite but its handler can
// (basecamp-mcp-server's list_projects and its signed next_cursor) cuts
// itself before it gets here, with FitsWhole and FitLeading: whole items, and
// a continuation minted at the first item cut.
//
// The cap covers everything the result carries twice: JSON restates its
// text as structuredContent, so when both are present each gets half.
//
// A listing that takes an offset names the offset it was served at, and its
// page when it has one, as top-level "offset" and "page" beside its list (the
// page/offset contract; see pageContinuationOf); cutting its list
// adds truncation.continue, the page and offset of the first item cut,
// because those listings have no page size to shrink — re-requesting would
// cut the same way, and following next_page would skip the cut items. An
// item too large to fit even alone is named in truncation.oversized_item
// (by position, and id when it has one) and the continuation
// steps past it, so continuing always makes progress.

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

// oversizedHint replaces the hint when the next item exceeds the limit alone.
const oversizedHint = "The next item exceeds the server's size limit on its own and cannot be returned; truncation.oversized_item names it by position and id. truncation.continue, when present, resumes after it."

// MinResultBytes is the smallest cap enforced: room for the truncation
// envelope and a useful amount of payload. A smaller positive setting is
// raised to it.
const MinResultBytes = 4096

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

// Cap enforces maxBytes on a successful result's text and structured
// content together. It returns the result unchanged when maxBytes <= 0, the
// result is an error (an error must reach the caller whole), or it already
// fits.
func Cap(result *mcp.CallToolResult, maxBytes int) *mcp.CallToolResult {
	if result == nil || result.IsError || maxBytes <= 0 {
		return result
	}
	maxBytes = max(maxBytes, MinResultBytes)
	idx, text := -1, ""
	for i, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			idx, text = i, tc.Text
			break
		}
	}
	if idx < 0 {
		return result
	}
	size, budget := len(text), maxBytes
	if result.StructuredContent != nil {
		structured, err := json.Marshal(result.StructuredContent)
		if err == nil {
			size += len(structured)
		}
		budget = maxBytes / 2
	}
	if size <= maxBytes {
		return result
	}
	limit := maxBytes
	maxBytes = budget

	var capped []byte
	var value any
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.UseNumber()
	if err := dec.Decode(&value); err == nil && !dec.More() {
		capped = capJSON(value, size, maxBytes, limit)
	}

	out := *result
	out.Content = append([]mcp.Content(nil), result.Content...)
	if capped == nil {
		out.Content[idx] = &mcp.TextContent{Text: capText(text, size, limit)}
		// Structured content restated the text; a cut text with a whole
		// structured copy would defeat the cap.
		out.StructuredContent = nil
		return &out
	}
	out.Content[idx] = &mcp.TextContent{Text: string(capped)}
	if result.StructuredContent != nil {
		out.StructuredContent = json.RawMessage(capped)
	}
	return &out
}

// capJSON renders value within maxBytes: compact if that fits, else with its
// largest list shortened, else as a flagged prefix of its compact text.
func capJSON(value any, originalBytes, maxBytes, limit int) []byte {
	compact, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	if len(compact) <= maxBytes {
		return compact
	}
	if out := shortenLargestList(value, originalBytes, maxBytes, limit); out != nil {
		return out
	}
	if m, ok := value.(map[string]any); ok {
		if ordered, err := prefixOrder(m); err == nil {
			compact = ordered
		}
	}
	return prefixJSON(string(compact), originalBytes, maxBytes, limit)
}

// prefixOrder renders an object compactly for the prefix fallback: its
// arrays and objects first, largest first, then its scalars, smallest first.
// A prefix ends inside whatever it cuts, so a scalar in it — a cursor, a
// position, a next URL — is there only if every array and object is there
// whole, and a continuation beside whole lists skips nothing. Equal sizes
// keep name order, so the rendering is deterministic.
func prefixOrder(m map[string]any) ([]byte, error) {
	type field struct {
		key, value []byte
		structured bool
	}
	fields := make([]field, 0, len(m))
	for k, v := range m {
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		_, isList := v.([]any)
		_, isObject := v.(map[string]any)
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
}

// FitsWhole reports whether v, served as JSON(v), passes Cap
// without a cut: a result is cut only when its compact rendering is over the
// share of the cap the text and its structured copy each get. maxBytes <= 0
// means no cap. A handler that owns a continuation this layer cannot rewrite
// sizes its own page with it (see FitLeading).
func FitsWhole(v any, maxBytes int) bool {
	if maxBytes <= 0 {
		return true
	}
	data, err := Marshal(v, false)
	return err == nil && len(data) <= max(maxBytes, MinResultBytes)/2
}

// JSONBytes is what Cap measures for JSON(v): the indented
// text and its compact structured copy together.
func JSONBytes(v any) int {
	indented, err := Marshal(v, true)
	if err != nil {
		return 0
	}
	compact, err := Marshal(v, false)
	if err != nil {
		return len(indented)
	}
	return len(indented) + len(compact)
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
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// shortenLargestList cuts the payload's largest list to the most leading
// items that fit, or returns nil when there is no list or even an empty one
// leaves the payload too large.
func shortenLargestList(value any, originalBytes, maxBytes, limit int) []byte {
	var (
		items []any
		field string
		build func(kept []any, t truncation) map[string]any
		cont  func(returned int) *pageContinuation
	)
	switch v := value.(type) {
	case []any:
		items, field = v, "items"
		build = func(kept []any, t truncation) map[string]any {
			return map[string]any{"items": kept, "truncated": true, "truncation": t}
		}
	case map[string]any:
		field = largestArrayField(v)
		arr, ok := v[field].([]any)
		if !ok {
			return nil
		}
		cont = pageContinuationOf(v)
		if cont == nil && !bareList(v) {
			// No offset contract: this object may carry a continuation
			// (a cursor, a position) that would point past a cut. Leave it
			// to the prefix path, which keeps no structured continuation.
			return nil
		}
		items = arr
		build = func(kept []any, t truncation) map[string]any {
			out := make(map[string]any, len(v)+2)
			for k, val := range v {
				out[k] = val
			}
			out[field] = kept
			out["truncated"] = true
			out["truncation"] = t
			return out
		}
	default:
		return nil
	}

	total := len(items)
	// render keeps the first k items; oversized marks item k as too large to
	// fit alone, so the continuation steps past it.
	render := func(k int, oversized bool) []byte {
		t := truncation{
			LimitBytes: limit, OriginalBytes: originalBytes, Field: field,
			Returned: &k, Total: &total, Hint: truncationHint,
		}
		next := k
		if cont != nil {
			t.Hint = continueHint
		}
		if oversized {
			t.OversizedItem = &oversizedItem{Index: k, ID: itemID(items[k])}
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

	// Size each item once, then pick the largest prefix whose estimate fits;
	// the estimate is exact up to the digits of the returned count, so step
	// down from it until the real rendering fits.
	empty := render(0, false)
	if empty == nil || len(empty) > maxBytes {
		return nil
	}
	budget := maxBytes - len(empty)
	k := 0
	for k < total {
		size, err := json.Marshal(items[k])
		if err != nil {
			return nil
		}
		cost := len(size)
		if k > 0 {
			cost++ // separating comma
		}
		if cost > budget {
			break
		}
		budget -= cost
		k++
	}
	if k == 0 && total > 0 {
		// The next item does not fit even alone: name it and step past it,
		// rather than answer with a continuation that returns to it forever.
		if out := render(0, true); out != nil && len(out) <= maxBytes {
			return out
		}
		return empty
	}
	// The estimate cannot see digit growth in the rendered counts and
	// continuation, so the real rendering may overshoot by a few bytes. Step
	// down, but never to zero items: a zero-item page whose continuation
	// points back at the same offset would repeat forever. If not even one
	// item fits, it is treated as oversized, and the continuation steps past it.
	for ; k >= 1; k-- {
		if out := render(k, false); out != nil && len(out) <= maxBytes {
			return out
		}
	}
	if total > 0 {
		if out := render(0, true); out != nil && len(out) <= maxBytes {
			return out
		}
	}
	return empty
}

// bareList reports whether m is a list served alone: {"items": [...]} with
// nothing beside it, which is how JSON serves a top-level array. Like a
// bare array, it carries no continuation a cut could leave pointing past the
// items removed.
func bareList(m map[string]any) bool {
	_, isList := m[ItemsKey].([]any)
	return isList && len(m) == 1
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
// continuation after `next` items;
// nil for any other payload.
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

// largestArrayField names the array-valued top-level field with the largest
// rendering, or "" when there is none. Ties go to the first name in order, so
// the choice is deterministic.
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

// prefixJSON wraps a prefix of the compact text in a flagged envelope sized
// to fit.
func prefixJSON(compact string, originalBytes, maxBytes, limit int) []byte {
	t := truncation{LimitBytes: limit, OriginalBytes: originalBytes, Hint: truncationHint}
	keep := maxBytes
	for keep > 0 {
		data, err := json.Marshal(map[string]any{
			"truncated":    true,
			"truncation":   t,
			"partial_json": utf8Prefix(compact, keep),
		})
		if err != nil {
			return nil
		}
		if len(data) <= maxBytes {
			return data
		}
		keep -= len(data) - maxBytes
	}
	return nil
}

// capText keeps a prefix of plain text and appends a note saying so.
func capText(text string, originalBytes, limit int) string {
	maxBytes := limit
	note := fmt.Sprintf("\n\n[truncated: showing the first part of a %d-byte result, cut to the server's %d-byte limit. %s]",
		originalBytes, limit, truncationHint)
	// MinResultBytes leaves room for the note; the prefix bound holds even
	// if it did not.
	if len(note) > maxBytes {
		return utf8Prefix(note, maxBytes)
	}
	return utf8Prefix(text, maxBytes-len(note)) + note
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
