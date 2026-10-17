// Package result shapes and bounds MCP tool results the way every 37signals
// server serves them: structuredContent always a JSON object (a list as
// {"items": [...]}), and a size cap that cuts an oversized result where the
// cut can be explained, with an exact continuation when the listing declares
// the page/offset contract. Middleware applies both, plus title mirroring on
// tools/list, to everything a server answers.
//
// Extracted from basecamp-mcp-server internal/tools (result_shape.go,
// result_cap.go) and hey-mcp-server's copy of the shape rules
// (internal/server/wire_shape.go).
package result
