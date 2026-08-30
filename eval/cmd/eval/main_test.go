package main

import (
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
