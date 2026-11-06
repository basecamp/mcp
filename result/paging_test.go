package result

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/mcptest"
)

// pagedServer serves three tools behind PagingMiddleware: get_record, a
// record read; list_things, a page-based listing whose handler takes no
// offset; and plain, named in neither. Each answers with answer(tool, args),
// and the arguments every handler saw are recorded.
type pagedServer struct {
	session *mcp.ClientSession
	mu      sync.Mutex
	seen    []map[string]any
}

func servePaged(t *testing.T, maxBytes int, answer func(tool string, args map[string]any) *mcp.CallToolResult) *pagedServer {
	t.Helper()
	ps := &pagedServer{}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	for _, name := range []string{"get_record", "list_things", "plain"} {
		schema := map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"id": map[string]any{"type": "integer"}, "page": map[string]any{"type": "string"}},
			"additionalProperties": false,
		}
		server.AddTool(&mcp.Tool{Name: name, InputSchema: schema}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if len(req.Params.Arguments) > 0 {
				require.NoError(t, json.Unmarshal(req.Params.Arguments, &args))
			}
			ps.mu.Lock()
			ps.seen = append(ps.seen, args)
			ps.mu.Unlock()
			return answer(req.Params.Name, args), nil
		})
	}
	server.AddReceivingMiddleware(PagingMiddleware(maxBytes, Paging{
		Records:  func(tool string) bool { return tool == "get_record" },
		Listings: func(tool string) string {
			if tool == "list_things" {
				return "items"
			}
			return ""
		},
	}))
	ps.session = mcptest.Connect(t, server)
	return ps
}

func (ps *pagedServer) call(t *testing.T, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := ps.session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	require.NoError(t, err)
	return res
}

// richText is text several times any cap here, full of multibyte runes and
// characters JSON escapes, so a window's bytes and its wire size differ.
func richText(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "<p>Line %d: «héllo» \"quoted\" \\ tab\t日本語 😀</p>\n", i)
	}
	return b.String()
}

// A record read pages its rich text: every part fits, every other field is
// whole in every part, nested and top-level fields page in step, and the
// parts rejoin byte for byte. Run at the smallest cap and a typical one.
func TestPaging_RecordPartsRejoin(t *testing.T) {
	description := richText(24_000)
	html := richText(18_000)
	plain := strings.Repeat("plain words ", 1_000)
	record := map[string]any{
		"id": json.Number("90071992547409931"), "title": "A long card", "url": "https://app.example.com/cards/1",
		"description": description,
		"body":        map[string]any{"html": html, "plain_text": plain},
		"tags":        []any{"a", "b"},
	}
	for _, maxBytes := range []int{MinBytes, 20_000} {
		t.Run(fmt.Sprint(maxBytes), func(t *testing.T) {
			ps := servePaged(t, maxBytes, func(string, map[string]any) *mcp.CallToolResult { return JSON(record) })
			got := map[string]string{}
			offset, parts := 0, 0
			for {
				args := map[string]any{"id": 1}
				if offset > 0 {
					args[ContentOffsetKey] = offset
				}
				res := ps.call(t, "get_record", args)
				require.False(t, res.IsError, resultText(t, res))
				assert.LessOrEqual(t, wireLen(t, res), max(maxBytes, MinBytes), "the result as the client got it, SDK additions included")
				part := decode(t, resultText(t, res))
				assert.Equal(t, part, structured(t, res), "text and structured copy agree")
				assert.Equal(t, true, part["truncated"])
				assert.Contains(t, resultText(t, res), "90071992547409931", "ids stay exact")
				assert.Equal(t, "A long card", part["title"])
				assert.Equal(t, []any{"a", "b"}, part["tags"])
				tr := asMap(t, part["truncation"])
				assert.EqualValues(t, offset, tr["offset"])
				fields := asMap(t, tr["fields"])
				assert.EqualValues(t, len(description), fields["description"])
				assert.EqualValues(t, len(html), fields["body.html"])
				assert.EqualValues(t, len(plain), fields["body.plain_text"])
				body := asMap(t, part["body"])
				for name, text := range map[string]any{"description": part["description"], "body.html": body["html"], "body.plain_text": body["plain_text"]} {
					got[name] += text.(string)
				}
				parts++
				require.Less(t, parts, 500, "paging must end")
				cont, more := tr["continue"].(map[string]any)
				if !more {
					assert.Equal(t, contentEndHint, tr["hint"])
					break
				}
				next := int(cont[ContentOffsetKey].(float64))
				require.Greater(t, next, offset, "each part advances")
				offset = next
			}
			assert.Greater(t, parts, 1)
			assert.Equal(t, description, got["description"])
			assert.Equal(t, html, got["body.html"])
			assert.Equal(t, plain, got["body.plain_text"])
			for _, args := range ps.seen {
				assert.NotContains(t, args, ContentOffsetKey, "the handler never sees content_offset")
			}
		})
	}
}

// A record that fits is served as it was, and a record with no rich text
// still gets the cap's own cut.
func TestPaging_RecordThatFitsIsUntouched(t *testing.T) {
	record := map[string]any{"id": 1, "description": "short"}
	ps := servePaged(t, MinBytes, func(string, map[string]any) *mcp.CallToolResult { return JSON(record) })
	res := ps.call(t, "get_record", map[string]any{"id": 1})
	assert.Equal(t, decode(t, mustJSON(t, record)), decode(t, resultText(t, res)))
	assert.NotContains(t, resultText(t, res), "truncated")
}

// content_offset is refused, before the handler runs, when it is not an
// integer >= 0; and after it, past the end of the rich text or on a result
// with none.
func TestPaging_ContentOffsetRefusals(t *testing.T) {
	record := map[string]any{"id": 1, "description": strings.Repeat("d", 100)}
	ps := servePaged(t, MinBytes, func(string, map[string]any) *mcp.CallToolResult { return JSON(record) })
	for _, bad := range []any{-1, 1.5, "3", true, json.RawMessage("1.0000000000000001"), json.RawMessage("1e400")} {
		res := ps.call(t, "get_record", map[string]any{ContentOffsetKey: bad})
		assert.True(t, res.IsError, "%v", bad)
		assert.Contains(t, resultText(t, res), "content_offset must be an integer >= 0")
	}
	assert.Empty(t, ps.seen, "an invalid offset never reaches the handler")

	res := ps.call(t, "get_record", map[string]any{ContentOffsetKey: 100})
	assert.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "past the end")

	none := servePaged(t, MinBytes, func(string, map[string]any) *mcp.CallToolResult { return JSON(map[string]any{"id": 1}) })
	res = none.call(t, "get_record", map[string]any{ContentOffsetKey: 5})
	assert.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "has none")

	// content_offset 0 is the default, and a whole number in any spelling is
	// a number.
	for _, zero := range []any{0.0, json.RawMessage("0e5"), json.RawMessage("0.000")} {
		res = ps.call(t, "get_record", map[string]any{ContentOffsetKey: zero})
		assert.False(t, res.IsError, "%v", zero)
	}
	n, ok := nonNegativeInt(json.Number("3e1"))
	assert.True(t, ok)
	assert.Equal(t, 30, n)
}

// A later part that fits whole is still flagged, with the end hint and no
// continuation.
func TestPaging_LastPartNamesItsOffset(t *testing.T) {
	record := map[string]any{"id": 1, "content": strings.Repeat("c", 10_000)}
	ps := servePaged(t, MinBytes, func(string, map[string]any) *mcp.CallToolResult { return JSON(record) })
	res := ps.call(t, "get_record", map[string]any{ContentOffsetKey: 9_000})
	got := decode(t, resultText(t, res))
	assert.Equal(t, strings.Repeat("c", 1_000), got["content"])
	tr := asMap(t, got["truncation"])
	assert.NotContains(t, tr, "continue")
	assert.EqualValues(t, 9_000, tr["offset"])
	assert.EqualValues(t, 1_000, tr["returned_bytes"])
}

// The middleware declares the argument it owns on the tools it pages, and
// on no other; a schema that declares it already keeps its own.
func TestPaging_AdvertisesItsArguments(t *testing.T) {
	ps := servePaged(t, MinBytes, func(string, map[string]any) *mcp.CallToolResult { return Text("ok") })
	tools := mcptest.ListTools(t, ps.session)
	props := func(name string) map[string]any {
		return asMap(t, asMap(t, decode(t, mustJSON(t, tools[name].InputSchema)))["properties"])
	}
	assert.Contains(t, props("get_record"), ContentOffsetKey)
	assert.NotContains(t, props("get_record"), OffsetKey)
	assert.Contains(t, props("list_things"), OffsetKey)
	assert.NotContains(t, props("list_things"), ContentOffsetKey)
	assert.NotContains(t, props("plain"), OffsetKey)
	assert.NotContains(t, props("plain"), ContentOffsetKey)
	assert.Contains(t, props("get_record"), "id", "the tool's own properties stay")

	exact := json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","enum":[9007199254740993]}}}`)
	copied, added := withProperty(exact, OffsetKey, "theirs")
	require.True(t, added)
	assert.Contains(t, mustJSON(t, copied), "9007199254740993", "numbers in the copied schema stay exact")

	declared := map[string]any{"type": "object", "properties": map[string]any{OffsetKey: map[string]any{"type": "integer", "description": "mine"}}}
	_, added = withProperty(declared, OffsetKey, "theirs")
	assert.False(t, added)
}

// thing is one listing item: an id, a link, and a body of n bytes.
func thing(id, n int) map[string]any {
	return map[string]any{"id": id, "app_url": fmt.Sprintf("https://app.example.com/things/%d", id), "body": strings.Repeat("b", n)}
}

// A HEY-shaped listing ({"items": [...], "next_page": cursor}, paged by an
// opaque cursor, no offset of its own, another list beside the one paged) is cut to whole items and continued
// within its page: following truncation.continue reads every item of the
// page exactly once, an item too large alone is named by its app_url and
// stepped past, and next_page survives for after.
func TestPaging_ListingContinuesWithinItsPage(t *testing.T) {
	pages := map[string][]any{}
	for p, cursor := range []string{"", "CUR2"} {
		var items []any
		for i := range 40 {
			n := 300 + 37*i
			if i == 17 {
				n = 20_000
			}
			items = append(items, thing(100*p+i, n))
		}
		pages[cursor] = items
	}
	ps := servePaged(t, MinBytes*2, func(_ string, args map[string]any) *mcp.CallToolResult {
		cursor, _ := args["page"].(string)
		// A second list beside the one paged, as a thread's participants
		// sit beside its messages, is served whole.
		out := map[string]any{"items": pages[cursor], "participants": []any{"ann", "bo"}}
		if cursor == "" {
			out["next_page"] = "CUR2"
		}
		return JSON(out)
	})
	for _, cursor := range []string{"", "CUR2"} {
		var read []any
		var oversized []any
		offset := 0
		for calls := 0; ; calls++ {
			require.Less(t, calls, 200, "paging must end")
			args := map[string]any{}
			if cursor != "" {
				args["page"] = cursor
			}
			if offset > 0 {
				args[OffsetKey] = offset
			}
			res := ps.call(t, "list_things", args)
			require.False(t, res.IsError, resultText(t, res))
			assert.LessOrEqual(t, wireLen(t, res), MinBytes*2)
			got := decode(t, resultText(t, res))
			assert.Equal(t, got, structured(t, res))
			assert.NotContains(t, got, "partial_json")
			read = append(read, asList(t, got["items"])...)
			assert.Equal(t, []any{"ann", "bo"}, got["participants"])
			if cursor == "" {
				assert.Equal(t, "CUR2", got["next_page"], "next_page is kept for after the page")
			}
			tr, cut := got["truncation"].(map[string]any)
			if !cut {
				break
			}
			if item, ok := tr["oversized_item"].(map[string]any); ok {
				oversized = append(oversized, item)
			}
			cont := asMap(t, tr["continue"])
			if cursor == "" {
				assert.NotContains(t, cont, "page", "a first page asked for without one is continued by offset alone")
				if tr["oversized_item"] == nil {
					assert.Equal(t, firstPageHint, tr["hint"])
				}
			} else {
				assert.Equal(t, cursor, cont["page"])
				if tr["oversized_item"] == nil {
					assert.Equal(t, continueHint, tr["hint"])
				}
			}
			next := int(cont[OffsetKey].(float64))
			require.Greater(t, next, offset)
			offset = next
		}
		var ids []float64
		for _, item := range read {
			ids = append(ids, asMap(t, item)["id"].(float64))
		}
		base := 0.0
		if cursor == "CUR2" {
			base = 100
		}
		var want []float64
		for i := range 40 {
			if i != 17 {
				want = append(want, base+float64(i))
			}
		}
		assert.Equal(t, want, ids, "page %q: every item read once, in order, but the oversized one", cursor)
		require.Len(t, oversized, 1)
		item := asMap(t, oversized[0])
		assert.EqualValues(t, base+17, item["id"])
		assert.Equal(t, fmt.Sprintf("https://app.example.com/things/%d", int(base)+17), item["app_url"])
	}
	for _, args := range ps.seen {
		assert.NotContains(t, args, OffsetKey, "the handler never sees offset")
	}
}

// A listing that fits at offset 0 is served as the handler built it; an
// offset on a result with no list is refused.
func TestPaging_ListingEdges(t *testing.T) {
	small := map[string]any{"items": []any{thing(1, 10)}, "next_page": "CUR2"}
	ps := servePaged(t, MinBytes, func(string, map[string]any) *mcp.CallToolResult { return JSON(small) })
	res := ps.call(t, "list_things", nil)
	assert.Equal(t, decode(t, mustJSON(t, small)), decode(t, resultText(t, res)))

	res = ps.call(t, "list_things", map[string]any{OffsetKey: 1})
	got := decode(t, resultText(t, res))
	assert.Empty(t, asList(t, got["items"]), "an offset past the page serves no items")
	assert.EqualValues(t, 1, got[OffsetKey])

	record := servePaged(t, MinBytes, func(string, map[string]any) *mcp.CallToolResult { return JSON(map[string]any{"id": 1}) })
	res = record.call(t, "list_things", map[string]any{OffsetKey: 2})
	assert.True(t, res.IsError)
}

// A tool named in neither keeps the cap as it was: a cursor-bearing listing
// still gets the prefix, and an offset argument reaches the handler (here,
// refused by its own schema).
func TestPaging_UnnamedToolsUnchanged(t *testing.T) {
	big := map[string]any{"items": []any{thing(1, 3_000), thing(2, 3_000)}, "next_page": "CUR2"}
	ps := servePaged(t, MinBytes, func(string, map[string]any) *mcp.CallToolResult { return JSON(big) })
	got := decode(t, resultText(t, ps.call(t, "plain", nil)))
	assert.NotEmpty(t, got["partial_json"])
}

// Cap continues a cut listing at a cursor page as it does a numbered one.
func TestCap_CursorPageContinuation(t *testing.T) {
	r := JSON(map[string]any{"items": bigItems(100), "page": "CUR2", "offset": 3, "next_page": "CUR3"})
	got := assertExactCut(t, Cap(r, 10_000), 10_000)
	tr := asMap(t, got["truncation"])
	cont := asMap(t, tr["continue"])
	assert.Equal(t, "CUR2", cont["page"])
	assert.EqualValues(t, 3+len(asList(t, got["items"])), cont["offset"])
	assert.Equal(t, continueHint, tr["hint"])
}

// A numeric page is named back in the spelling it came in.
func TestCap_NumericPageKeepsItsSpelling(t *testing.T) {
	for _, page := range []string{"2", "2.0", "2e0"} {
		r := Text(`{"items":` + mustJSON(t, bigItems(100)) + `,"page":` + page + `,"offset":0}`)
		out := Cap(r, 10_000)
		assert.Contains(t, resultText(t, out), `"continue":{"page":`+page+`,"offset":`, page)
	}
}

// An oversized item whose link would push the step past it over the limit
// is stepped past without it.
func TestCap_OversizedItemShedsItsLink(t *testing.T) {
	items := []any{
		map[string]any{"id": 7, "app_url": "https://example.com/" + strings.Repeat("u", 3_400), "body": strings.Repeat("x", 20_000)},
		map[string]any{"id": 8},
	}
	got := assertExactCut(t, Cap(JSON(map[string]any{"items": items, "offset": 0}), MinBytes), MinBytes)
	tr := asMap(t, got["truncation"])
	assert.NotContains(t, asMap(t, tr["oversized_item"]), "app_url")
	assert.EqualValues(t, 1, asMap(t, tr["continue"])["offset"])
}
