package result

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/mcptest"
)

func bigItems(n int) []map[string]any {
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = map[string]any{"id": i, "title": strings.Repeat("x", 200)}
	}
	return items
}

func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, r.Content)
	tc, ok := r.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return tc.Text
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &m), s)
	return m
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	require.True(t, ok, "%T", v)
	return m
}

func asList(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	require.True(t, ok, "%T", v)
	return l
}

// resultBytes is what the cap bounds: the text plus the structured content.
func resultBytes(t *testing.T, r *mcp.CallToolResult) int {
	t.Helper()
	n := len(resultText(t, r))
	if r.StructuredContent != nil {
		data, err := json.Marshal(r.StructuredContent)
		require.NoError(t, err)
		n += len(data)
	}
	return n
}

// A result under the cap is returned as is.
func TestCapResult_UnderCapUntouched(t *testing.T) {
	r := JSON(map[string]any{"a": 1})
	assert.Same(t, r, Cap(r, 10_000))
	assert.Same(t, r, Cap(r, 0), "0 disables the cap")
}

// Errors reach the caller whole, whatever their size.
func TestCapResult_ErrorsUntouched(t *testing.T) {
	r := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("e", 50_000)}}}
	assert.Same(t, r, Cap(r, MinBytes))
}

// The cap counts the structured copy JSON carries: text and structured
// content together stay within it.
func TestCapResult_CountsStructuredContent(t *testing.T) {
	r := JSON(map[string]any{"items": bigItems(50)})
	text := resultText(t, r)
	require.Less(t, len(text), 20_000, "the text alone fits")
	require.Greater(t, resultBytes(t, r), 20_000, "text and structured together do not")

	out := Cap(r, 20_000)
	assert.LessOrEqual(t, resultBytes(t, out), 20_000)
	assert.Equal(t, json.RawMessage(resultText(t, out)), out.StructuredContent, "structured content matches the text")
}

// Indentation alone over the cap: compact rendering fits, nothing is lost,
// nothing is flagged.
func TestCapResult_CompactFitsWithoutTruncation(t *testing.T) {
	payload := map[string]any{"items": bigItems(20)}
	r := JSON(payload)
	compact, err := json.Marshal(payload)
	require.NoError(t, err)
	limit := 2 * len(compact)
	require.Greater(t, resultBytes(t, r), limit)

	out := Cap(r, limit)
	got := decode(t, resultText(t, out))
	assert.NotContains(t, got, "truncated")
	assert.Len(t, got["items"], 20)
}

// An object without the offset contract may carry a continuation this layer
// cannot rewrite (a cursor, the event feed's position and next). Shortening
// its list beside an untouched cursor would let a caller resume past the
// cut, so it keeps a flagged prefix instead — and the prefix, which a model
// reads, must not carry the continuation either, wherever its name sorts.
func TestCapResult_CursorBearingObjectKeepsNoContinuation(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"next_cursor": {"projects": bigItems(100), "next_cursor": "CURSOR-VALUE", "total_count": 300},
		"feed":        {"events": bigItems(100), "position": "POSITION-VALUE", "next": "https://x/events?position=NEXT-VALUE"},
		"sorts first": {"zitems": bigItems(100), "a_next": "CURSOR-VALUE", "meta": map[string]any{"n": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			out := Cap(JSON(payload), 10_000)
			assert.LessOrEqual(t, resultBytes(t, out), 10_000)
			got := decode(t, resultText(t, out))
			assert.Equal(t, true, got["truncated"])
			partial, _ := got["partial_json"].(string)
			assert.NotEmpty(t, partial)
			for _, key := range []string{"next_cursor", "position", "next", "a_next"} {
				assert.NotContains(t, got, key, "no structured continuation survives a cut")
			}
			for _, value := range []string{"CURSOR-VALUE", "POSITION-VALUE", "NEXT-VALUE"} {
				assert.NotContains(t, partial, value, "no continuation survives in the prefix")
			}
			assert.NotContains(t, asMap(t, got["truncation"]), "continue")
		})
	}
}

// The prefix renders lists and objects first, largest first, then scalars,
// smallest first: a scalar shows only once every list before it is whole.
func TestPrefixOrder(t *testing.T) {
	got, err := prefixOrder(map[string]any{
		"a_cursor": "abcdef",
		"count":    json.Number("3"),
		"small":    []any{1},
		"big":      []any{1, 2, 3},
		"obj":      map[string]any{"k": "v"},
	})
	require.NoError(t, err)
	assert.Equal(t, `{"obj":{"k":"v"},"big":[1,2,3],"small":[1],"count":3,"a_cursor":"abcdef"}`, string(got))
}

// FitLeading finds the most items that fit, and 0 when none does.
func TestFitLeading(t *testing.T) {
	for _, tc := range []struct{ n, limit, want int }{
		{0, 5, 0}, {10, 10, 10}, {10, 20, 10}, {10, 7, 7}, {10, 0, 0}, {1, 0, 0}, {100, 1, 1},
	} {
		assert.Equal(t, tc.want, FitLeading(tc.n, func(k int) bool { return k <= tc.limit }), "%+v", tc)
	}
}

// A top-level array keeps as many leading items as fit, wrapped so the flag
// has somewhere to live; nothing beside it can point past the cut.
func TestCapResult_ShortensTopLevelArray(t *testing.T) {
	out := Cap(JSON(bigItems(100)), 10_000)
	assert.LessOrEqual(t, resultBytes(t, out), 10_000)
	text := resultText(t, out)
	got := decode(t, text)
	assert.Equal(t, true, got["truncated"])
	kept := asList(t, got["items"])
	require.NotEmpty(t, kept)
	assert.EqualValues(t, 0, asMap(t, kept[0])["id"], "the leading items are kept")

	tr := asMap(t, got["truncation"])
	assert.Equal(t, "items", tr["field"])
	assert.EqualValues(t, len(kept), tr["returned"])
	assert.EqualValues(t, 100, tr["total"])
	assert.EqualValues(t, 10_000, tr["limit_bytes"])
	assert.NotEmpty(t, tr["hint"])

	// As many as fit: one more item would not.
	one, err := json.Marshal(kept[0])
	require.NoError(t, err)
	assert.Greater(t, len(text)+len(one)+1, 10_000/2)
}

// A cut page of a page-numbered listing names the page and offset that
// continue it, counting from the offset it was served at.
func TestCapResult_PageListingContinuation(t *testing.T) {
	r := JSON(map[string]any{"Bookmarks": bigItems(100), "page": 3, "offset": 10, "next_page": 4})
	got := decode(t, resultText(t, Cap(r, 10_000)))
	kept := asList(t, got["Bookmarks"])
	tr := asMap(t, got["truncation"])
	cont := asMap(t, tr["continue"])
	assert.EqualValues(t, 3, cont["page"])
	assert.EqualValues(t, 10+len(kept), cont["offset"])
	assert.Contains(t, tr["hint"], "truncation.continue")
	assert.EqualValues(t, 4, got["next_page"])
}

// No list to shorten: a flagged prefix of the compact JSON.
func TestCapResult_PrefixWhenNoList(t *testing.T) {
	out := Cap(JSON(map[string]any{"body": strings.Repeat("é", 10_000)}), 5_000)
	assert.LessOrEqual(t, resultBytes(t, out), 5_000)
	got := decode(t, resultText(t, out))
	assert.Equal(t, true, got["truncated"])
	assert.NotEmpty(t, got["partial_json"])
}

// A positive cap below the minimum is raised to it, so the truncation
// envelope always fits within what is emitted.
func TestCapResult_SmallCapRaisedToMinimum(t *testing.T) {
	for _, r := range []*mcp.CallToolResult{
		JSON(map[string]any{"items": bigItems(100)}),
		textResult(strings.Repeat("a", 50_000)),
	} {
		out := Cap(r, 10)
		assert.LessOrEqual(t, resultBytes(t, out), MinBytes)
		assert.Contains(t, resultText(t, out), "truncated")
	}
}

// Plain text keeps a prefix and a note; resource links and _meta survive.
func TestCapResult_PlainTextAndOtherContent(t *testing.T) {
	link := &mcp.ResourceLink{URI: "basecamp://x", Name: "x"}
	r := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", 50_000)}, link},
		Meta:    mcp.Meta{"ui": map[string]any{"resourceUri": "ui://x"}},
	}
	out := Cap(r, 5_000)
	text := resultText(t, out)
	assert.LessOrEqual(t, len(text), 5_000)
	assert.Contains(t, text, "[truncated:")
	assert.Same(t, link, out.Content[1])
	assert.Equal(t, r.Meta, out.Meta)
	assert.Len(t, resultText(t, r), 50_000, "the handler's result is not mutated")
}

// The middleware applies the shape rule and the cap to every call, over the
// wire, whatever the handler returned.
func TestMiddleware_ShapesAndCapsEveryCall(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "big", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: mustMarshal(t, bigItems(100))}},
				StructuredContent: bigItems(100),
			}, nil
		})
	srv.AddReceivingMiddleware(Middleware(8_000))
	session := mcptest.Connect(t, srv)

	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "big", Arguments: map[string]any{}})
	require.NoError(t, err)
	assert.LessOrEqual(t, resultBytes(t, out), 8_000)
	got := decode(t, resultText(t, out))
	assert.Equal(t, true, got["truncated"])
	assert.NotEmpty(t, asList(t, got["items"]), "a bare list is cut to whole leading items")
	_, isObject := out.StructuredContent.(map[string]any)
	assert.True(t, isObject, "structuredContent is an object: %T", out.StructuredContent)
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// An item too large to fit alone is named, and the continuation steps past
// it: continuing always makes progress.
func TestCapResult_OversizedItemAdvancesContinuation(t *testing.T) {
	items := []map[string]any{
		{"id": 7, "body": strings.Repeat("x", 20_000)},
		{"id": 8, "body": "small"},
	}
	r := JSON(map[string]any{"Drafts": items, "page": 1, "offset": 4})
	got := decode(t, resultText(t, Cap(r, 10_000)))
	assert.Empty(t, asList(t, got["Drafts"]))
	tr := asMap(t, got["truncation"])
	assert.EqualValues(t, 7, asMap(t, tr["oversized_item"])["id"])
	cont := asMap(t, tr["continue"])
	assert.EqualValues(t, 1, cont["page"])
	assert.EqualValues(t, 5, cont["offset"], "the continuation steps past the oversized item")
}

// An offset-only listing (no page) continues by offset alone, and
// original_bytes reports what the result carried in total.
func TestCapResult_OffsetOnlyContinuationAndOriginalBytes(t *testing.T) {
	r := JSON(map[string]any{"items": bigItems(100), "offset": 0})
	before := resultBytes(t, r)
	got := decode(t, resultText(t, Cap(r, 10_000)))
	tr := asMap(t, got["truncation"])
	cont := asMap(t, tr["continue"])
	assert.NotContains(t, cont, "page")
	assert.EqualValues(t, len(asList(t, got["items"])), cont["offset"])
	assert.EqualValues(t, before, tr["original_bytes"], "text and structured content together")
}

// Whatever the budget, a cut listing's continuation always moves forward
// from the offset it was served at: never a zero-item page pointing back at
// itself.
func TestCapResult_ContinuationAlwaysAdvances(t *testing.T) {
	for limit := MinBytes; limit < MinBytes+600; limit += 7 {
		for _, offset := range []int{0, 9, 99, 999} {
			items := make([]map[string]any, 30)
			for i := range items {
				items[i] = map[string]any{"id": i, "body": strings.Repeat("y", 1500+i)}
			}
			r := JSON(map[string]any{"Drafts": items, "page": 1, "offset": offset})
			out := Cap(r, limit)
			got := decode(t, resultText(t, out))
			tr := asMap(t, got["truncation"])
			cont, ok := tr["continue"].(map[string]any)
			require.True(t, ok, "limit %d offset %d: a cut page names its continuation", limit, offset)
			assert.Greater(t, cont["offset"], float64(offset), "limit %d offset %d: continuation must advance", limit, offset)
			assert.LessOrEqual(t, resultBytes(t, out), limit)
		}
	}
}
