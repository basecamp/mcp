package filecontent

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// WithoutFile is a file tool as a client other than ChatGPT is listed it: a
// copy, with no file argument, content_base64 required in its place, and no
// _meta["openai/fileParams"]. The schema is copied through JSON, so nothing
// the served definition holds is changed.
//
// A product lists ChatGPT the tool as served and every other client this,
// and refuses a call from one that sends file anyway (Options.AcceptsFile
// false), so a client is listed the argument exactly when its calls take it.
func WithoutFile(t *mcp.Tool) *mcp.Tool {
	out := *t
	raw, err := json.Marshal(t.InputSchema)
	var schema map[string]any
	if err != nil || json.Unmarshal(raw, &schema) != nil {
		return &out
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		delete(props, Param)
	}
	// file is never required where content_base64 stands in for it.
	required, _ := schema["required"].([]any)
	required = slices.DeleteFunc(required, func(name any) bool { return name == Param })
	if !slices.Contains(required, any("content_base64")) {
		required = append(required, "content_base64")
	}
	schema["required"] = required
	out.InputSchema = schema
	if _, ok := t.Meta[MetaKey]; ok {
		meta := maps.Clone(t.Meta)
		delete(meta, MetaKey)
		if len(meta) == 0 {
			meta = nil
		}
		out.Meta = meta
	}
	return &out
}
