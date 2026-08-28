// Package mcp is the root of the 37signals MCP toolkit: machinery shared by
// our MCP servers (basecamp-mcp-server, hey-mcp-server), extracted once two
// product instances have proven it by duplication.
//
// The root package holds no code; the toolkit arrives as subpackages
// (mcptest, gateway, catalog) via the extraction sequence tracked on the MCP
// program board. Products depend on this module; this module never imports a
// product SDK or server.
package mcp
