// Package result shapes and bounds MCP tool results the way every 37signals
// server serves them: structuredContent always a JSON object (a list as
// {"items": [...]}), a content array that is never null, and a size cap on
// the serialized result that cuts an oversized one where the cut can be
// explained — exactly, with page/offset continuations, for the one-JSON-block
// shape every server serves, and coarsely, with a notice that always fits,
// for any other. Middleware applies both, plus title mirroring on
// tools/list, to everything a server answers.
//
// Extracted from basecamp-mcp-server internal/tools (result_shape.go,
// result_cap.go), hey-mcp-server's copy of the shape rules
// (internal/server/wire_shape.go) and its never-null content array, and
// fizzy-mcp-server's need for the same cap. The cap here measures the
// serialized result where basecamp-mcp-server's copy sums text lengths, and
// gives shapes other than one JSON text block the coarse cut.
package result
