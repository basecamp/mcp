package catalog

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/gateway"
	"github.com/basecamp/mcp/mcptest"
)

// fixtureModel is the hand-written model snapshot under testdata: a small
// fictional product with three tags — Notes and Search are curated into
// domains, Admin is deliberately left unclaimed.
func fixtureModel() fs.FS {
	return os.DirFS("testdata/model")
}

// testSpec stands in for a product's parameterization of the catalog.
func testSpec(model fs.FS) Spec {
	return Spec{
		ToolPrefix: "test_",
		Domains: []DomainSpec{
			{Key: "notes", Blurb: "Notes under test: list, create, get, delete.", Tags: []string{"Notes"}},
			{Key: "search", Blurb: "Search the notes under test.", Tags: []string{"Search"}},
		},
		Model: model,
	}
}

func load(t *testing.T) *Catalog {
	t.Helper()
	cat, err := Load(testSpec(fixtureModel()))
	require.NoError(t, err, "catalog must derive cleanly from the fixture model")
	return cat
}

func domainByKey(t *testing.T, cat *Catalog, key string) *Domain {
	t.Helper()
	for _, d := range cat.Domains {
		if d.Key == key {
			return d
		}
	}
	t.Fatalf("no domain %q in catalog", key)
	return nil
}

// mutatedModel rewrites the fixture through a mutation, for exercising the
// fail-closed paths without a second testdata snapshot.
func mutatedModel(t *testing.T, mutate func(bm, oa map[string]any)) fs.FS {
	t.Helper()
	read := func(name string) map[string]any {
		data, err := fs.ReadFile(fixtureModel(), name)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(data, &m))
		return m
	}
	bm := read("behavior-model.json")
	oa := read("openapi.json")
	mutate(bm, oa)

	write := func(m map[string]any) *fstest.MapFile {
		data, err := json.Marshal(m)
		require.NoError(t, err)
		return &fstest.MapFile{Data: data}
	}
	return fstest.MapFS{
		"behavior-model.json": write(bm),
		"openapi.json":        write(oa),
	}
}

// addOperation registers one extra operation in both model files.
func addOperation(bm, oa map[string]any, path, id, tag string) {
	bm["operations"].(map[string]any)[id] = map[string]any{"readonly": true, "idempotent": true}
	oa["paths"].(map[string]any)[path] = map[string]any{
		"get": map[string]any{
			"operationId": id,
			"tags":        []any{tag},
			"description": "Does a thing.",
		},
	}
}

func TestLoadRequiresModel(t *testing.T) {
	_, err := Load(Spec{ToolPrefix: "test_"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Spec.Model is required")
}

func TestLoadDerivesAllDomains(t *testing.T) {
	cat := load(t)
	assert.Equal(t, "fixture-1", cat.ModelVersion)
	require.Len(t, cat.Domains, 2)
	for _, d := range cat.Domains {
		assert.NotEmpty(t, d.Operations, "domain %q has no operations", d.Key)
		assert.Equal(t, "test_"+d.Key, d.Tool)
		assert.NotEmpty(t, d.Blurb, "domain %q has no blurb", d.Key)
	}
	assert.Equal(t, []string{"create_note", "delete_note", "get_note", "list_notes"},
		domainByKey(t, cat, "notes").ActionNames())
}

func TestActionNamesAreWellFormedAndUnique(t *testing.T) {
	wellFormed := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	cat := load(t)
	for _, d := range cat.Domains {
		seen := map[string]bool{gateway.DescribeAction: true}
		for _, op := range d.Operations {
			assert.Regexp(t, wellFormed, op.Action, "operation %s", op.ID)
			assert.False(t, seen[op.Action], "duplicate action %q in domain %q", op.Action, d.Key)
			seen[op.Action] = true
		}
	}
}

// TestLoadRejectsTornSnapshots proves the join is strict both ways: an
// operation present in one model file but not the other refuses to build.
func TestLoadRejectsTornSnapshots(t *testing.T) {
	t.Run("openapi only", func(t *testing.T) {
		model := mutatedModel(t, func(bm, oa map[string]any) {
			delete(bm["operations"].(map[string]any), "ListNotes")
		})
		_, err := Load(testSpec(model))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `operation "ListNotes" in openapi.json but not behavior-model.json`)
	})

	t.Run("behavior model only", func(t *testing.T) {
		model := mutatedModel(t, func(bm, oa map[string]any) {
			bm["operations"].(map[string]any)["GhostOp"] = map[string]any{"readonly": true}
		})
		_, err := Load(testSpec(model))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `operation "GhostOp" in behavior-model.json but not openapi.json`)
	})
}

func TestLoadRejectsDuplicateOperationIDs(t *testing.T) {
	model := mutatedModel(t, func(bm, oa map[string]any) {
		oa["paths"].(map[string]any)["/notes-dup"] = map[string]any{
			"get": map[string]any{"operationId": "ListNotes", "tags": []any{"Notes"}},
		}
	})
	_, err := Load(testSpec(model))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate operationId "ListNotes"`)
}

func TestLoadRejectsMultiTagOperations(t *testing.T) {
	model := mutatedModel(t, func(bm, oa map[string]any) {
		get := oa["paths"].(map[string]any)["/search"].(map[string]any)["get"].(map[string]any)
		get["tags"] = []any{"Search", "Notes"}
	})
	_, err := Load(testSpec(model))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `operation "SearchNotes": expected exactly one tag`)
}

func TestLoadRejectsTagClaimedTwice(t *testing.T) {
	spec := testSpec(fixtureModel())
	spec.Domains = append(spec.Domains, DomainSpec{Key: "extra", Blurb: "Claims Notes again.", Tags: []string{"Notes"}})
	_, err := Load(spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `tag "Notes" claimed by both "notes" and "extra"`)
}

func TestLoadRejectsNullOperations(t *testing.T) {
	// A null operation unmarshals cleanly; the join must refuse it rather
	// than panic on the nil dereference during startup.
	model := mutatedModel(t, func(bm, oa map[string]any) {
		oa["paths"].(map[string]any)["/notes-null"] = map[string]any{"get": nil}
	})
	_, err := Load(testSpec(model))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "null operation")
}

func TestLoadRejectsDuplicateDomainKeys(t *testing.T) {
	// Two specs sharing a key would both survive Load but collapse to one
	// entry in gateway.New's name index, silently dropping operations.
	spec := testSpec(fixtureModel())
	spec.Domains = append(spec.Domains, DomainSpec{Key: spec.Domains[0].Key, Blurb: "Same key again.", Tags: []string{"Tasks"}})
	_, err := Load(spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate domain key")
}

func TestLoadRejectsRequestBodyRefs(t *testing.T) {
	// The reusable Reference Object form would otherwise unmarshal to an
	// empty body and silently drop the schema and required flag.
	model := mutatedModel(t, func(bm, oa map[string]any) {
		post := oa["paths"].(map[string]any)["/notes"].(map[string]any)["post"].(map[string]any)
		post["requestBody"] = map[string]any{"$ref": "#/components/requestBodies/NewNote"}
	})
	_, err := Load(testSpec(model))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `requestBody $ref is not supported`)
}

func TestLoadRejectsTagsAbsentFromModel(t *testing.T) {
	// A misspelled or stale spec tag must not publish a hollow domain while
	// the real operations quietly land in Unmapped.
	spec := testSpec(fixtureModel())
	spec.Domains = append(spec.Domains, DomainSpec{Key: "ghost", Blurb: "Claims nothing real.", Tags: []string{"Nootes"}})
	_, err := Load(spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `domain "ghost" claims tag "Nootes", which is not in the model`)
}

func TestLoadRejectsActionNameCollisions(t *testing.T) {
	// "List_notes" snake-cases to "list_notes", colliding with "ListNotes".
	model := mutatedModel(t, func(bm, oa map[string]any) {
		addOperation(bm, oa, "/notes-collide", "List_notes", "Notes")
	})
	_, err := Load(testSpec(model))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `domain "notes": action "list_notes"`)
	assert.Contains(t, err.Error(), "collides with")
}

func TestLoadRejectsReservedDescribeAction(t *testing.T) {
	model := mutatedModel(t, func(bm, oa map[string]any) {
		addOperation(bm, oa, "/describe", "Describe", "Notes")
	})
	_, err := Load(testSpec(model))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `domain "notes": action "describe" from Describe collides with (reserved)`)
}

// TestUnmappedTagsAreAccounted pins the fixture tag the curated domains
// deliberately leave unexposed: unclaimed operations are counted, not lost.
func TestUnmappedTagsAreAccounted(t *testing.T) {
	cat := load(t)
	assert.Equal(t, map[string][]string{"Admin": {"PurgeAccount"}}, cat.Unmapped)
}

// TestEveryModelOperationIsMappedOrAccounted proves the catalog is total over
// the model: each operation lands in exactly one domain or the unmapped
// report — none silently dropped.
func TestEveryModelOperationIsMappedOrAccounted(t *testing.T) {
	var bm behaviorModel
	require.NoError(t, unmarshalModel(fixtureModel(), "behavior-model.json", &bm))

	cat := load(t)
	placed := map[string]int{}
	for _, d := range cat.Domains {
		for _, op := range d.Operations {
			placed[op.ID]++
		}
	}
	for _, ids := range cat.Unmapped {
		for _, id := range ids {
			placed[id]++
		}
	}

	assert.Len(t, placed, len(bm.Operations))
	for id := range bm.Operations {
		assert.Equal(t, 1, placed[id], "operation %q placed %d times", id, placed[id])
	}
}

func TestReadOnlyDerivesFromBehaviorModel(t *testing.T) {
	var bm behaviorModel
	require.NoError(t, unmarshalModel(fixtureModel(), "behavior-model.json", &bm))

	cat := load(t)
	for _, d := range cat.Domains {
		for _, op := range d.Operations {
			assert.Equal(t, bm.Operations[op.ID].ReadOnly, op.ReadOnly, "operation %s", op.ID)
			assert.Equal(t, bm.Operations[op.ID].Idempotent, op.Idempotent, "operation %s", op.ID)
			assert.Equal(t, bm.Operations[op.ID].Pagination != nil, op.Paginated, "operation %s", op.ID)
		}
	}
}

func TestFilterReadOnlyDropsWrites(t *testing.T) {
	spec := testSpec(fixtureModel())
	spec.Domains = append(spec.Domains, DomainSpec{Key: "admin", Blurb: "Admin ops under test.", Tags: []string{"Admin"}})
	cat, err := Load(spec)
	require.NoError(t, err)

	// notes mixes reads and writes: the copy keeps only the reads.
	filtered, ok := domainByKey(t, cat, "notes").FilterReadOnly()
	require.True(t, ok)
	notes := filtered.(*Domain)
	assert.True(t, notes.AllReadOnly())
	assert.Equal(t, []string{"get_note", "list_notes"}, notes.ActionNames())

	// search is all read-only already.
	filtered, ok = domainByKey(t, cat, "search").FilterReadOnly()
	require.True(t, ok)
	assert.Equal(t, []string{"search_notes"}, filtered.(*Domain).ActionNames())

	// admin is write-only: nothing remains.
	filtered, ok = domainByKey(t, cat, "admin").FilterReadOnly()
	assert.False(t, ok)
	assert.Nil(t, filtered)
}

func TestDescribeResolvesBodySchema(t *testing.T) {
	cat := load(t)
	notes := domainByKey(t, cat, "notes")

	payload, err := notes.Describe("create_note")
	require.NoError(t, err)
	op, ok := payload.(*Operation)
	require.True(t, ok)
	require.NotNil(t, op.Body, "CreateNote has a JSON request body")
	assert.NotContains(t, op.Body, "$ref", "body schema $ref should be inlined")
	assert.Contains(t, op.Body, "properties")

	// The nested $ref (NewNote.author -> Author) is inlined too.
	author, ok := op.Body["properties"].(map[string]any)["author"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, author, "$ref")
	assert.Contains(t, author, "properties")

	_, err = notes.Describe("no_such_action")
	assert.Error(t, err)

	whole, err := notes.Describe("")
	require.NoError(t, err)
	assert.Contains(t, whole.(map[string]any), "actions")
}

// TestParamRefSiblingsOverlayResolvedTarget asserts sibling keys alongside a
// $ref land on top of the resolved target, and codegen extensions are dropped
// from both.
func TestParamRefSiblingsOverlayResolvedTarget(t *testing.T) {
	cat := load(t)
	op, ok := domainByKey(t, cat, "notes").Operation("list_notes")
	require.True(t, ok)

	var filter *Param
	for i := range op.Params {
		if op.Params[i].Name == "filter" {
			filter = &op.Params[i]
		}
	}
	require.NotNil(t, filter, "ListNotes has a filter param")

	assert.Equal(t, []any{"all", "starred", "archived"}, filter.Schema["enum"], "resolved target's keys survive")
	assert.Equal(t, "Filter to apply.", filter.Schema["description"], "sibling key overlays the target's")
	assert.NotContains(t, filter.Schema, "$ref")
	assert.NotContains(t, filter.Schema, "x-go-type")
	assert.NotContains(t, filter.Schema, "x-go-name")
}

// TestDescribeSchemasAreSelfContained walks every derived parameter and body
// schema and asserts no local $ref survives resolution (siblings included)
// and no x-go-* codegen extension leaks into what MCP clients see.
func TestDescribeSchemasAreSelfContained(t *testing.T) {
	var assertClean func(t *testing.T, context string, v any)
	assertClean = func(t *testing.T, context string, v any) {
		switch tv := v.(type) {
		case map[string]any:
			for k, val := range tv {
				assert.NotEqual(t, "$ref", k, "unresolved $ref in %s", context)
				assert.False(t, strings.HasPrefix(k, "x-go-"), "codegen extension %q in %s", k, context)
				assertClean(t, context, val)
			}
		case []any:
			for _, val := range tv {
				assertClean(t, context, val)
			}
		}
	}

	cat := load(t)
	for _, d := range cat.Domains {
		for _, op := range d.Operations {
			for _, p := range op.Params {
				assertClean(t, op.ID+" param "+p.Name, p.Schema)
			}
			if op.Body != nil {
				assertClean(t, op.ID+" body", op.Body)
			}
		}
	}
}

// TestSummariesAreCompleteLines guards against hard-wrapped SDK descriptions
// truncating mid-sentence into the generated tool docs.
func TestRecursiveRefsTruncateSelfContained(t *testing.T) {
	// TreeNode references itself; inlining must terminate in a stub that
	// carries no $ref, since describe never returns the components object.
	cat := load(t)
	notes := domainByKey(t, cat, "notes")
	payload, err := notes.Describe("create_note")
	require.NoError(t, err)
	op, ok := payload.(*Operation)
	require.True(t, ok)
	rendered, err := json.Marshal(op.Body)
	require.NoError(t, err)
	assert.NotContains(t, string(rendered), "$ref")
	assert.Contains(t, string(rendered), "truncated: recursive reference to TreeNode")
}

func TestSummariesAreCompleteLines(t *testing.T) {
	cat := load(t)
	for _, d := range cat.Domains {
		for _, op := range d.Operations {
			assert.NotEmpty(t, op.Summary, "operation %s", op.ID)
			assert.NotContains(t, op.Summary, "\n", "operation %s", op.ID)
			assert.NotRegexp(t, `[,:;]$|\s(a|an|and|the|of|to|is|or)$`, op.Summary,
				"operation %s summary looks truncated: %q", op.ID, op.Summary)
		}
	}

	// ListNotes has no summary and a hard-wrapped description: the summary is
	// the first sentence of the first paragraph, unwrapped.
	notes := domainByKey(t, cat, "notes")
	list, ok := notes.Operation("list_notes")
	require.True(t, ok)
	assert.Equal(t, "Lists the notes in the account, newest first", list.Summary)

	// SearchNotes declares an explicit summary, which wins.
	search, ok := domainByKey(t, cat, "search").Operation("search_notes")
	require.True(t, ok)
	assert.Equal(t, "Search notes by query", search.Summary)
}

// TestBodyRequiredMirrorsModel asserts describe payloads carry whether the
// request body itself is mandatory, exactly as the OpenAPI export declares it.
// The body schema's inner "required" array cannot express this: it constrains
// properties only once a body is supplied.
func TestBodyRequiredMirrorsModel(t *testing.T) {
	var oa openapiDoc
	require.NoError(t, unmarshalModel(fixtureModel(), "openapi.json", &oa))
	declared := map[string]bool{}
	for _, methods := range oa.Paths {
		for _, op := range methods {
			declared[op.OperationID] = op.RequestBody != nil && op.RequestBody.Required
		}
	}

	cat := load(t)
	sawRequired := false
	for _, d := range cat.Domains {
		for _, op := range d.Operations {
			assert.Equal(t, declared[op.ID], op.BodyRequired,
				"operation %s body_required drifted from the model", op.ID)
			sawRequired = sawRequired || op.BodyRequired
		}
	}
	require.True(t, sawRequired, "model has no body-required operations; test is vacuous")
}

// TestServesThroughGateway proves the derived catalog satisfies gateway.Domain
// end to end: gateway.New over GatewayDomains, one wire round-trip through
// mcptest for tools/list, describe, and a dispatched action.
func TestServesThroughGateway(t *testing.T) {
	cat := load(t)
	srv, err := gateway.New(cat.GatewayDomains(), gateway.Config{
		Handler: func(ctx context.Context, d gateway.Domain, op gateway.Operation, params map[string]any) (*mcp.CallToolResult, error) {
			return gateway.JSONResult(map[string]any{"domain": d.Name(), "action": op.Action, "params": params})
		},
	})
	require.NoError(t, err)

	impl := &mcp.Implementation{Name: "catalog-test", Version: "0.0.0"}
	session := mcptest.Connect(t, srv.BuildMCPServer(impl, slog.New(slog.DiscardHandler)))

	tools := mcptest.ListTools(t, session)
	require.Len(t, tools, 2)
	require.Contains(t, tools, "test_notes")
	require.Contains(t, tools, "test_search")
	assert.Contains(t, tools["test_notes"].Description, "- create_note: Creates a note")
	assert.True(t, tools["test_search"].Annotations.ReadOnlyHint)
	assert.False(t, tools["test_notes"].Annotations.ReadOnlyHint)

	// Describe serves the full joined operation over the wire.
	text, isError := mcptest.CallText(t, session, "test_notes", map[string]any{
		"action": "describe",
		"params": map[string]any{"action": "create_note"},
	})
	require.False(t, isError, "describe failed: %s", text)
	var described Operation
	require.NoError(t, json.Unmarshal([]byte(text), &described))
	assert.Equal(t, "create_note", described.Action)
	assert.True(t, described.BodyRequired)
	assert.NotNil(t, described.Body)

	// Dispatch resolves catalog operations into the product handler.
	text, isError = mcptest.CallText(t, session, "test_notes", map[string]any{
		"action": "get_note",
		"params": map[string]any{"id": "1"},
	})
	require.False(t, isError, "dispatch failed: %s", text)
	assert.Contains(t, text, `"action": "get_note"`)
	assert.Contains(t, text, `"domain": "notes"`)
}
