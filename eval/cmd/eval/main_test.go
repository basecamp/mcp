package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{in: "/tmp/fizzy-mcp stdio --writes", want: []string{"/tmp/fizzy-mcp", "stdio", "--writes"}},
		{in: `"/opt/my apps/fizzy-mcp" stdio`, want: []string{"/opt/my apps/fizzy-mcp", "stdio"}},
		{in: "  spaced   out  ", want: []string{"spaced", "out"}},
		{in: "", err: true},
		{in: "   ", err: true},
		{in: `bad "unbalanced`, err: true},
	}
	for _, tc := range cases {
		got, err := splitCommand(tc.in)
		if tc.err {
			if err == nil {
				t.Fatalf("splitCommand(%q): want error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("splitCommand(%q): unexpected error %v", tc.in, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("splitCommand(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPreflightWritable(t *testing.T) {
	dir := t.TempDir()
	// A fresh path in a writable directory passes and is created empty.
	fresh := filepath.Join(dir, "out.jsonl")
	if err := preflightWritable(fresh); err != nil {
		t.Fatalf("writable path rejected: %v", err)
	}
	if info, err := os.Stat(fresh); err != nil || info.Size() != 0 {
		t.Fatalf("preflight did not create an empty file: info=%v err=%v", info, err)
	}
	// An existing file keeps its contents: preflight must never truncate.
	if err := os.WriteFile(fresh, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := preflightWritable(fresh); err != nil {
		t.Fatalf("existing path rejected: %v", err)
	}
	if data, _ := os.ReadFile(fresh); string(data) != "keep\n" {
		t.Fatalf("preflight truncated the existing file: %q", data)
	}
	// A missing directory fails before any run.
	if err := preflightWritable(filepath.Join(dir, "missing", "out.jsonl")); err == nil {
		t.Fatal("path in a missing directory accepted")
	}
}

func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	if !sameFile(a, a) {
		t.Fatal("a path does not alias itself")
	}
	if !sameFile(a, link) {
		t.Fatal("a symlink alias was not detected")
	}
	if sameFile(a, b) {
		t.Fatal("distinct files reported as one")
	}
	if sameFile(a, filepath.Join(dir, "missing.jsonl")) || sameFile(a, "") {
		t.Fatal("a missing or empty path aliased a file")
	}
}

func TestServerFieldsHonorsEnvOverride(t *testing.T) {
	t.Setenv("EVAL_HEY_CMD", "/custom/hey-mcp stdio --writes")
	got, err := serverFields("hey", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"/custom/hey-mcp", "stdio", "--writes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("env override: got %v want %v", got, want)
	}
	if _, err := serverFields("nonesuch", ""); err == nil {
		t.Fatalf("unknown server must error")
	}
}

func TestServerFieldsSplitsExplicitCommand(t *testing.T) {
	got, err := serverFields("hey", `"/opt/my apps/hey-mcp" stdio`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A spaced path in an explicit command survives via quotes; the registry
	// path avoids the join/split round-trip entirely.
	if want := []string{"/opt/my apps/hey-mcp", "stdio"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit cmd: got %v want %v", got, want)
	}
}

func TestChildEnvInjectsDummyTokenWhenAbsent(t *testing.T) {
	t.Setenv("FIZZY_TOKEN", "")
	env := childEnv("fizzy")
	found := false
	for _, kv := range env {
		if kv == "FIZZY_TOKEN=eval-structural-only" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fizzy childEnv must inject a dummy FIZZY_TOKEN")
	}
}

func TestChildEnvNeverOverwritesRealToken(t *testing.T) {
	t.Setenv("FIZZY_TOKEN", "real-secret")
	for _, kv := range childEnv("fizzy") {
		if kv == "FIZZY_TOKEN=eval-structural-only" {
			t.Fatalf("childEnv overwrote a real FIZZY_TOKEN")
		}
	}
}

func TestServerProfilesRecordHermeticity(t *testing.T) {
	for _, name := range []string{"fizzy", "hey"} {
		if !serverProfiles[name].hermetic {
			t.Fatalf("%s should be hermetic", name)
		}
	}
	if serverProfiles["basecamp"].hermetic {
		t.Fatalf("basecamp stdio authenticates eagerly; it must be marked non-hermetic")
	}
}

// TestCheckAliases pins which flag pairs may not name one file: the write
// would destroy the other. The two allowed overlaps — compare-then-overwrite
// (--out over --baseline) and rewriting a loaded corpus in place — must stay
// allowed.
func TestCheckAliases(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	out, corpus, results := mk("out.jsonl"), mk("corpus.json"), mk("prior.jsonl")
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(results, link); err != nil {
		t.Fatal(err)
	}

	if err := checkAliases(out, corpus, "", results); err != nil {
		t.Fatalf("distinct files refused: %v", err)
	}
	// --out must not be the source corpus or the corpus being written.
	if err := checkAliases(corpus, "", corpus, ""); err == nil {
		t.Fatal("--out aliasing --scenarios accepted")
	}
	if err := checkAliases(out, out, "", ""); err == nil {
		t.Fatal("--out aliasing --write-scenarios accepted")
	}
	// --write-scenarios must not replace the baseline, through a symlink too.
	if err := checkAliases(out, results, "", results); err == nil {
		t.Fatal("--write-scenarios aliasing --baseline accepted")
	}
	if err := checkAliases(out, link, "", results); err == nil {
		t.Fatal("--write-scenarios aliasing --baseline via symlink accepted")
	}
	// Allowed: compare-then-overwrite, and rewriting a loaded corpus in place.
	if err := checkAliases(results, "", "", results); err != nil {
		t.Fatalf("--out over --baseline must stay allowed: %v", err)
	}
	if err := checkAliases(out, corpus, corpus, ""); err != nil {
		t.Fatalf("rewriting the loaded corpus in place must stay allowed: %v", err)
	}
}
