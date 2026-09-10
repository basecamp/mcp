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
