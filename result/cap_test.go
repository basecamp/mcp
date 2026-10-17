package result

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

// wireLen is what the cap bounds: the result as the SDK serializes it.
func wireLen(t *testing.T, r *mcp.CallToolResult) int {
	t.Helper()
	data, err := json.Marshal(r)
	require.NoError(t, err)
	return len(data)
}

// structured decodes a result's structured content as an object.
func structured(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.NotNil(t, r.StructuredContent)
	return decode(t, mustJSON(t, r.StructuredContent))
}

// assertExactCut checks a cut in the standard shape: within the limit on the
// wire, flagged, its structured copy (when restated) the same JSON as its text.
func assertExactCut(t *testing.T, out *mcp.CallToolResult, limit int) map[string]any {
	t.Helper()
	assert.LessOrEqual(t, wireLen(t, out), limit-frameReserve)
	require.Len(t, out.Content, 1)
	text := resultText(t, out)
	got := decode(t, text)
	assert.Equal(t, true, got["truncated"])
	if out.StructuredContent != nil {
		assert.JSONEq(t, text, mustJSON(t, out.StructuredContent), "the structured copy restates the text")
	}
	return got
}

// A result under the cap is returned as is.
func TestCap_UnderCapUntouched(t *testing.T) {
	r := JSON(map[string]any{"a": 1})
	assert.Same(t, r, Cap(r, 10_000))
	assert.Same(t, r, Cap(r, 0), "0 disables the cap")
	assert.Same(t, r, Cap(r, -1), "a negative cap disables it too")
}

// Errors reach the caller whole, whatever their size.
func TestCap_ErrorsUntouched(t *testing.T) {
	r := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("e", 50_000)}}}
	assert.Same(t, r, Cap(r, MinBytes))
}

// The boundary is the serialized size plus the frame reserve: a result
// exactly at the limit passes whole, one byte over it is cut, and the cut
// fits.
func TestCap_BoundaryIsTheSerializedSize(t *testing.T) {
	r := JSON(map[string]any{"items": bigItems(40)})
	n := wireLen(t, r)
	require.Greater(t, n, MinBytes)

	assert.Same(t, r, Cap(r, n+frameReserve), "exactly at the limit: untouched")
	out := Cap(r, n+frameReserve-1)
	assert.NotSame(t, r, out)
	assert.LessOrEqual(t, wireLen(t, out), n-1)
	got := decode(t, resultText(t, out))
	assert.NotContains(t, got, "truncated", "compact rendering fits: nothing lost, nothing flagged")
	assert.Len(t, got["items"], 40)
}

// The cap counts the wire, not the text: escaping a string costs bytes the
// text length doesn't show. HTML-significant characters serialize as six
// bytes each, and quotes in the restated text as two, so a cut sized by
// string length would overshoot.
func TestCap_EscapingCountsAgainstTheLimit(t *testing.T) {
	items := make([]map[string]any, 200)
	for i := range items {
		items[i] = map[string]any{"id": i, "title": strings.Repeat(`<&>"\`, 60)}
	}
	for _, limit := range []int{8_000, 10_000, 50_000} {
		out := Cap(JSON(items), limit)
		got := assertExactCut(t, out, limit)
		assert.NotEmpty(t, asList(t, got["items"]), "limit %d", limit)
	}
}

// Indentation alone over the cap: compact rendering fits, nothing is lost,
// nothing is flagged.
func TestCap_CompactFitsWithoutTruncation(t *testing.T) {
	r := JSON(map[string]any{"items": bigItems(20)})
	compact := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: mustJSON(t, map[string]any{"items": bigItems(20)})}},
		StructuredContent: json.RawMessage(mustJSON(t, map[string]any{"items": bigItems(20)})),
	}
	limit := wireLen(t, compact) + frameReserve
	require.Greater(t, wireLen(t, r)+frameReserve, limit)

	out := Cap(r, limit)
	got := decode(t, resultText(t, out))
	assert.NotContains(t, got, "truncated")
	assert.Len(t, got["items"], 20)
}

// A top-level array keeps as many leading items as fit, and exactly as many:
// one more would not.
func TestCap_ShortensTopLevelArrayToTheMostThatFit(t *testing.T) {
	out := Cap(JSON(bigItems(100)), 10_000)
	got := assertExactCut(t, out, 10_000)
	kept := asList(t, got["items"])
	require.NotEmpty(t, kept)
	assert.EqualValues(t, 0, asMap(t, kept[0])["id"], "the leading items are kept")

	tr := asMap(t, got["truncation"])
	assert.Equal(t, "items", tr["field"])
	assert.EqualValues(t, len(kept), tr["returned"])
	assert.EqualValues(t, 100, tr["total"])
	assert.EqualValues(t, 10_000, tr["limit_bytes"])
	assert.EqualValues(t, JSONBytes(bigItems(100)), tr["original_bytes"])
	assert.NotEmpty(t, tr["hint"])

	// One more item, with its envelope, would not fit.
	value, _ := decodeJSON([]byte(mustJSON(t, bigItems(100))))
	all := value.([]any)
	k, total := len(kept)+1, 100
	more := mustJSON(t, map[string]any{"items": all[:k], "truncated": true, "truncation": truncation{
		LimitBytes: 10_000, OriginalBytes: JSONBytes(bigItems(100)), Field: "items", Returned: &k, Total: &total, Hint: truncationHint,
	}})
	assert.Greater(t, wireLen(t, &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: more}},
		StructuredContent: json.RawMessage(more),
	}), 10_000-frameReserve)
}

// JSON text with no structured copy (the gateway's describe) is the standard
// shape too: cut exactly, and given no structured content it didn't have.
func TestCap_JSONTextAlone(t *testing.T) {
	r := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(t, bigItems(100))}}}
	out := Cap(r, 10_000)
	got := assertExactCut(t, out, 10_000)
	assert.Nil(t, out.StructuredContent)
	assert.NotEmpty(t, asList(t, got["items"]))
}

// An object without the offset contract may carry a continuation this layer
// cannot rewrite (a cursor, the event feed's position and next). Shortening
// its list beside an untouched cursor would let a caller resume past the
// cut, so it keeps a flagged prefix instead — and the prefix, which a model
// reads, must not carry the continuation either, wherever its name sorts.
func TestCap_CursorBearingObjectKeepsNoContinuation(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"next_cursor": {"projects": bigItems(100), "next_cursor": "CURSOR-VALUE", "total_count": 300},
		"feed":        {"events": bigItems(100), "position": "POSITION-VALUE", "next": "https://x/events?position=NEXT-VALUE"},
		"sorts first": {"zitems": bigItems(100), "a_next": "CURSOR-VALUE", "meta": map[string]any{"n": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			got := assertExactCut(t, Cap(JSON(payload), 10_000), 10_000)
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

	// Nested objects, list items included, are ordered the same way.
	got, err = prefixOrder(map[string]any{
		"next": "N",
		"results": map[string]any{"box": map[string]any{
			"changes_url": "C", "postings": []any{map[string]any{"z": []any{1}, "a": "s"}},
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, `{"results":{"box":{"postings":[{"z":[1],"a":"s"}],"changes_url":"C"}},"next":"N"}`, string(got))
}

// A continuation nested beside the list it continues doesn't survive a
// prefix that cuts the list, however deep it sits.
func TestCap_NestedContinuationKeptOutOfThePrefix(t *testing.T) {
	box := map[string]any{"name": "Imbox", "posting_changes_url": "https://x/changes?since=SINCE-VALUE", "postings": bigItems(100)}
	for name, payload := range map[string]any{
		"one level":  map[string]any{"next_page": "CURSOR-VALUE", "results": box},
		"two levels": map[string]any{"next_page": "CURSOR-VALUE", "results": map[string]any{"box": box}},
	} {
		t.Run(name, func(t *testing.T) {
			got := assertExactCut(t, Cap(JSON(payload), 10_000), 10_000)
			partial, _ := got["partial_json"].(string)
			require.NotEmpty(t, partial)
			assert.NotContains(t, partial, "SINCE-VALUE")
			assert.NotContains(t, partial, "CURSOR-VALUE")
		})
	}
}

// FitLeading finds the most items that fit, and 0 when none does.
func TestFitLeading(t *testing.T) {
	for _, tc := range []struct{ n, limit, want int }{
		{0, 5, 0}, {10, 10, 10}, {10, 20, 10}, {10, 7, 7}, {10, 0, 0}, {1, 0, 0}, {100, 1, 1},
	} {
		assert.Equal(t, tc.want, FitLeading(tc.n, func(k int) bool { return k <= tc.limit }), "%+v", tc)
	}
}

// A cut page of a page-numbered listing names the page and offset that
// continue it, counting from the offset it was served at.
func TestCap_PageListingContinuation(t *testing.T) {
	r := JSON(map[string]any{"Bookmarks": bigItems(100), "page": 3, "offset": 10, "next_page": 4})
	got := assertExactCut(t, Cap(r, 10_000), 10_000)
	kept := asList(t, got["Bookmarks"])
	tr := asMap(t, got["truncation"])
	cont := asMap(t, tr["continue"])
	assert.EqualValues(t, 3, cont["page"])
	assert.EqualValues(t, 10+len(kept), cont["offset"])
	assert.Equal(t, continueHint, tr["hint"])
	assert.EqualValues(t, 4, got["next_page"])
}

// An offset-only listing (no page) continues by offset alone, with a hint
// that names no page and no next_page.
func TestCap_OffsetOnlyContinuation(t *testing.T) {
	got := assertExactCut(t, Cap(JSON(map[string]any{"items": bigItems(100), "offset": 0}), 10_000), 10_000)
	tr := asMap(t, got["truncation"])
	cont := asMap(t, tr["continue"])
	assert.NotContains(t, cont, "page")
	assert.EqualValues(t, len(asList(t, got["items"])), cont["offset"])
	assert.Equal(t, offsetHint, tr["hint"])
	assert.NotContains(t, tr["hint"], "next_page")
}

// An offset listing with two lists gets no synthesized continuation: the
// offset could belong to either.
func TestCap_AmbiguousOffsetListGetsNoContinuation(t *testing.T) {
	out := Cap(JSON(map[string]any{"items": bigItems(2), "facets": bigItems(80), "offset": 0}), 8_000)
	got := assertExactCut(t, out, 8_000)
	assert.NotContains(t, resultText(t, out), `"continue"`)
	assert.NotEmpty(t, got["partial_json"])
}

// No list to shorten: a flagged prefix of the compact JSON.
func TestCap_PrefixWhenNoList(t *testing.T) {
	got := assertExactCut(t, Cap(JSON(map[string]any{"body": strings.Repeat("é", 10_000)}), 5_000), 5_000)
	assert.NotEmpty(t, got["partial_json"])
}

// Escaping that inflates a prefix past its own length (backslashes double,
// and double again in the restated text) still finds the longest prefix that
// fits rather than giving up.
func TestCap_PrefixWithInflatingEscapes(t *testing.T) {
	for name, r := range map[string]*mcp.CallToolResult{
		"text and structured": JSON(map[string]any{"a": strings.Repeat(`\`, 10_000)}),
		"structured restated, compact": {
			Content:           []mcp.Content{&mcp.TextContent{Text: mustJSON(t, map[string]any{"a": strings.Repeat(`\`, 10_000)})}},
			StructuredContent: map[string]any{"a": strings.Repeat(`\`, 10_000)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := assertExactCut(t, Cap(r, MinBytes), MinBytes)
			assert.NotEmpty(t, got["partial_json"])
		})
	}
}

// An item too large to fit alone is named, and the continuation steps past
// it: continuing always makes progress.
func TestCap_OversizedItemAdvancesContinuation(t *testing.T) {
	items := []map[string]any{
		{"id": 7, "body": strings.Repeat("x", 20_000)},
		{"id": 8, "body": "small"},
	}
	got := assertExactCut(t, Cap(JSON(map[string]any{"Drafts": items, "page": 1, "offset": 4}), 10_000), 10_000)
	assert.Empty(t, asList(t, got["Drafts"]))
	tr := asMap(t, got["truncation"])
	assert.EqualValues(t, 7, asMap(t, tr["oversized_item"])["id"])
	cont := asMap(t, tr["continue"])
	assert.EqualValues(t, 1, cont["page"])
	assert.EqualValues(t, 5, cont["offset"], "the continuation steps past the oversized item")
}

// An item that would fit on its own, crowded out by the rest of the
// payload, is not oversized: skipping it would lose an item a caller could
// read. The cut carries no continuation past it.
func TestCap_CrowdedItemIsNotSkipped(t *testing.T) {
	items := []map[string]any{{"id": 7, "body": strings.Repeat("x", 3_000)}, {"id": 8, "body": "small"}}
	payload := map[string]any{"Drafts": items, "page": 1, "offset": 4, "note": strings.Repeat("n", 3_000)}
	got := assertExactCut(t, Cap(JSON(payload), 8_000), 8_000)
	tr := asMap(t, got["truncation"])
	assert.NotContains(t, tr, "oversized_item")
	assert.NotContains(t, tr, "continue")
}

// Whatever the budget, a cut listing's continuation always moves forward
// from the offset it was served at: never a zero-item page pointing back at
// itself.
func TestCap_ContinuationAlwaysAdvances(t *testing.T) {
	for limit := MinBytes; limit < MinBytes+600; limit += 7 {
		for _, offset := range []int{0, 9, 99, 999} {
			items := make([]map[string]any, 30)
			for i := range items {
				items[i] = map[string]any{"id": i, "body": strings.Repeat("y", 1500+i)}
			}
			out := Cap(JSON(map[string]any{"Drafts": items, "page": 1, "offset": offset}), limit)
			got := assertExactCut(t, out, limit)
			tr := asMap(t, got["truncation"])
			cont, ok := tr["continue"].(map[string]any)
			require.True(t, ok, "limit %d offset %d: a cut page names its continuation", limit, offset)
			assert.Greater(t, cont["offset"], float64(offset), "limit %d offset %d: continuation must advance", limit, offset)
		}
	}
}

// An oversized item whose id is itself too large to name is still stepped
// past, or the list isn't cut at all: never a zero-item page whose
// continuation points back at the same offset.
func TestCap_OversizedItemWithHugeID(t *testing.T) {
	items := []map[string]any{
		{"id": strings.Repeat("i", 5000), "body": strings.Repeat("x", 20_000)},
		{"id": 8, "body": "small"},
	}
	out := Cap(JSON(map[string]any{"Drafts": items, "page": 1, "offset": 4}), MinBytes)
	got := assertExactCut(t, out, MinBytes)
	if cont, ok := asMap(t, got["truncation"])["continue"].(map[string]any); ok {
		assert.Greater(t, cont["offset"], float64(4), "a continuation must advance")
	}
}

// A positive cap below the minimum is raised to it.
func TestCap_SmallCapRaisedToMinimum(t *testing.T) {
	out := Cap(JSON(map[string]any{"items": bigItems(100)}), 10)
	got := assertExactCut(t, out, MinBytes)
	assert.EqualValues(t, MinBytes, asMap(t, got["truncation"])["limit_bytes"])
}

// A cut text block keeps its _meta and annotations, and the result its _meta.
func TestCap_KeepsMetadata(t *testing.T) {
	meta := mcp.Meta{"k": "v"}
	ann := &mcp.Annotations{Audience: []mcp.Role{"user"}}
	text := mustJSON(t, bigItems(100))
	r := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text, Meta: meta, Annotations: ann}},
		Meta:    mcp.Meta{"ui": map[string]any{"resourceUri": "ui://x"}},
	}
	out := Cap(r, 10_000)
	assertExactCut(t, out, 10_000)
	tc := out.Content[0].(*mcp.TextContent)
	assert.Equal(t, meta, tc.Meta)
	assert.Equal(t, ann, tc.Annotations)
	assert.Equal(t, r.Meta, out.Meta)
	assert.Equal(t, text, resultText(t, r), "the handler's result is not mutated")
}

// Text that only starts like JSON (trailing bytes after a document) isn't the
// standard shape: it is not mistaken for its leading document.
func TestCap_TrailingBytesAreNotJSON(t *testing.T) {
	r := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "{}]" + strings.Repeat("a", 10_000)}}}
	out := Cap(r, MinBytes)
	assertCoarse(t, out, r, MinBytes)
}

func TestDecodeJSONRefusesTrailingBytes(t *testing.T) {
	for _, bad := range []string{`{}]`, `{"id":1}}`, `[]]`, `{} {}`, `{} x`, ``, `x`} {
		_, ok := decodeJSON([]byte(bad))
		assert.False(t, ok, bad)
	}
	_, ok := decodeJSON([]byte(" {\"a\": 1}\n"))
	assert.True(t, ok)
}

// assertCoarse checks the coarse cut: within the limit on the wire, one text
// notice naming the sizes, and structured content reduced to the same
// explanation when the result had any.
func assertCoarse(t *testing.T, out, in *mcp.CallToolResult, limit int) {
	t.Helper()
	assert.LessOrEqual(t, wireLen(t, out), limit-frameReserve)
	require.Len(t, out.Content, 1)
	text := resultText(t, out)
	assert.Contains(t, text, "[truncated:")
	assert.Contains(t, text, "withheld")
	assert.False(t, out.IsError, "a withheld result is still a success")
	if in.StructuredContent == nil {
		assert.Nil(t, out.StructuredContent)
		return
	}
	got := structured(t, out)
	assert.Equal(t, true, got["truncated"])
	tr := asMap(t, got["truncation"])
	assert.EqualValues(t, limit, tr["limit_bytes"])
	assert.Equal(t, withheldHint, tr["hint"])
}

// Every shape other than one JSON text block gets the coarse cut, within
// the limit.
func TestCap_OtherShapesGetTheCoarseCut(t *testing.T) {
	big := map[string]any{"items": bigItems(100)}
	cases := map[string]*mcp.CallToolResult{
		"plain text":                   {Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", 50_000)}}},
		"structured only":              {Content: []mcp.Content{}, StructuredContent: big},
		"structured beside prose":      {Content: []mcp.Content{&mcp.TextContent{Text: "Here they are."}}, StructuredContent: big},
		"structured beside a scalar":   {Content: []mcp.Content{&mcp.TextContent{Text: "42"}}, StructuredContent: big},
		"prose, then restated JSON":    {Content: []mcp.Content{&mcp.TextContent{Text: "Here."}, &mcp.TextContent{Text: mustJSON(t, big)}}, StructuredContent: big},
		"small text, then a large one": {Content: []mcp.Content{&mcp.TextContent{Text: "ok"}, &mcp.TextContent{Text: strings.Repeat("a", 50_000)}}},
		"two large texts":              {Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", 30_000)}, &mcp.TextContent{Text: strings.Repeat("b", 30_000)}}},
		"an image":                     {Content: []mcp.Content{&mcp.ImageContent{Data: make([]byte, 50_000), MIMEType: "image/png"}}},
		"JSON text beside an image":    {Content: []mcp.Content{&mcp.TextContent{Text: "{}"}, &mcp.ImageContent{Data: make([]byte, 50_000), MIMEType: "image/png"}}},
		"an embedded resource": {Content: []mcp.Content{&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
			URI: "x://y", Text: strings.Repeat("r", 50_000),
		}}}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			before := wireLen(t, in)
			out := Cap(in, 8_000)
			assertCoarse(t, out, in, 8_000)
			assert.Contains(t, resultText(t, out), mustJSON(t, before), "the notice names the original size")
			assert.Equal(t, before, wireLen(t, in), "the handler's result is not mutated")
		})
	}
}

// A standard-shape result whose _meta alone overflows the cap has no exact
// cut that fits; it gets the coarse cut, which drops the _meta.
func TestCap_HugeMetaFallsBackToCoarse(t *testing.T) {
	in := JSON(map[string]any{"a": 1})
	in.Meta = mcp.Meta{"blob": strings.Repeat("m", 20_000)}
	out := Cap(in, 8_000)
	assertCoarse(t, out, in, 8_000)
	assert.Nil(t, out.Meta)

	// _meta that fits beside the notice is kept.
	small := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", 50_000)}},
		Meta:    mcp.Meta{"ui": "x"},
	}
	assert.Equal(t, small.Meta, Cap(small, 8_000).Meta)
}

// The coarse cut depends only on two integers once _meta is dropped, so
// its size has a ceiling: at the largest integers it can name, with
// structured content, it sits well under MinBytes. _meta is kept only when
// the cut still fits with it. So every coarse cut fits under every limit Cap
// enforces.
func TestCapCoarse_AlwaysFitsTheLimit(t *testing.T) {
	ceiling := capCoarse(&mcp.CallToolResult{StructuredContent: map[string]any{"a": 1}}, math.MaxInt, math.MaxInt)
	assert.Less(t, wireLen(t, ceiling), MinBytes/4, "the ceiling sits well under MinBytes")

	for name, in := range map[string]*mcp.CallToolResult{
		"text":                 {Content: []mcp.Content{&mcp.TextContent{Text: "x"}}},
		"structured":           {StructuredContent: map[string]any{"a": 1}},
		"structured with meta": {StructuredContent: map[string]any{"a": 1}, Meta: mcp.Meta{"blob": strings.Repeat("m", 3_500)}},
		"huge meta":            {Meta: mcp.Meta{"blob": strings.Repeat("m", math.MaxUint16)}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, limit := range []int{MinBytes, MinBytes + 1, 10_000, 100_000} {
				out := capCoarse(in, math.MaxInt, limit)
				assert.LessOrEqual(t, wireLen(t, out), limit-frameReserve, "limit %d", limit)
			}
		})
	}
}
