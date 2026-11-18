package upstreamtest

import "testing"

// recorder is a testing.TB that records whether it was failed.
type recorder struct {
	testing.TB
	failed bool
}

func (r *recorder) Helper()               {}
func (r *recorder) Errorf(string, ...any) { r.failed = true }

func TestAssertClean(t *testing.T) {
	tests := []struct {
		name, text, secret string
		leaks              bool
	}{
		{"absent", "nothing here", Planted, false},
		{"whole", "token " + Planted, Planted, true},
		{"window", "token " + Planted[3:3+WindowBytes], Planted, true},
		{"split by invisibles", "token " + splitByInvisibles(Planted), Planted, true},
		{"short secret with a control", "abc\tdef", "abc\tdef", true},
		{"long secret with a control", "x" + "abcdef\tghijklmnop" + "x", "abcdef\tghijklmnop", true},
		{"secret with non-ASCII", "café-sécurité-token", "café-sécurité-token", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recorder{TB: t}
			AssertClean(r, "sink", tt.text, tt.secret)
			if r.failed != tt.leaks {
				t.Fatalf("AssertClean failed = %v, want %v", r.failed, tt.leaks)
			}
		})
	}
}
