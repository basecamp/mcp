package eval

import (
	"fmt"
	"strings"
)

// SplitCommand tokenizes a command line, honoring single and double quotes so a
// path or argument containing spaces survives intact. It rejects an empty
// command rather than indexing into no fields.
func SplitCommand(s string) ([]string, error) {
	var fields []string
	var cur strings.Builder
	inField := false
	var quote rune // 0, '\'' or '"'
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inField = true
		case r == ' ' || r == '\t' || r == '\n':
			if inField {
				fields = append(fields, cur.String())
				cur.Reset()
				inField = false
			}
		default:
			cur.WriteRune(r)
			inField = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced quote in command %q", s)
	}
	if inField {
		fields = append(fields, cur.String())
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty server command")
	}
	return fields, nil
}
