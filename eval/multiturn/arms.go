package multiturn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/eval/cassette"
)

// Arm is one guidance condition. Each arm is realized twice over, on purpose:
//
//   - Server side, by ServerArgs and ServerEnv — the flags or config the
//     server under test takes to switch that guidance on or off, applied when
//     the harness launches it for the arm.
//   - Client side, by the three switches — the harness passes the server's
//     instructions to the model only when Instructions is set, lists the guide
//     tools only when Guide is set, and preloads the skill only when Skill is
//     set.
//
// The client side is what makes an arm mean what it says against any server
// build: a server that always sends instructions still yields a true bare arm.
// The server side covers what a client cannot strip (richer tool descriptions,
// error hints) once the server gates them. And an arm that asks for guidance
// the server does not offer — +guide against a build with no guide tool — is
// refused before any spend rather than silently measured as bare.
type Arm struct {
	Name         string            `json:"name"`
	Instructions bool              `json:"instructions"`
	Guide        bool              `json:"guide"`
	Skill        bool              `json:"skill"`
	ServerArgs   []string          `json:"server_args,omitempty"`
	ServerEnv    map[string]string `json:"server_env,omitempty"`
}

// ArmSet is an arms file: the arms one server is evaluated under, plus where
// its guidance lives.
type ArmSet struct {
	Server string `json:"server"`
	// GuideTools are the tool names that make up the guide surface; they are
	// hidden from the model in every arm without Guide.
	GuideTools []string `json:"guide_tools"`
	// SkillResource is an MCP resource URI the server serves its skill at
	// (SEP-2640 skills are also readable as resources). Read at connect.
	SkillResource string `json:"skill_resource,omitempty"`
	// SkillFile is a local skill file, relative to the arms file — the plugin
	// case, where the skill ships beside the server rather than through it.
	// Used when SkillResource is unset.
	SkillFile string `json:"skill_file,omitempty"`
	Arms      []Arm  `json:"arms"`

	dir string
}

// LoadArms reads and validates an arms file.
func LoadArms(path string) (*ArmSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s ArmSet
	if err := cassette.DecodeStrict(data, &s); err != nil {
		return nil, fmt.Errorf("arms %s: %w", path, err)
	}
	s.dir = filepath.Dir(path)
	if len(s.Arms) == 0 {
		return nil, fmt.Errorf("arms %s: no arms", path)
	}
	seen := map[string]bool{}
	for _, a := range s.Arms {
		if strings.TrimSpace(a.Name) == "" || strings.ContainsAny(a.Name, "/ \t") {
			return nil, fmt.Errorf("arms %s: arm name %q must be a non-empty word", path, a.Name)
		}
		if seen[a.Name] {
			return nil, fmt.Errorf("arms %s: duplicate arm %q", path, a.Name)
		}
		seen[a.Name] = true
		if a.Guide && len(s.GuideTools) == 0 {
			return nil, fmt.Errorf("arms %s: arm %q wants the guide but guide_tools is empty", path, a.Name)
		}
		if s.SkillFile != "" && !filepath.IsLocal(s.SkillFile) {
			return nil, fmt.Errorf("arms %s: skill_file %q must be a path beneath the arms file's directory", path, s.SkillFile)
		}
		if a.Skill && s.SkillResource == "" && s.SkillFile == "" {
			return nil, fmt.Errorf("arms %s: arm %q wants the skill but neither skill_resource nor skill_file is set", path, a.Name)
		}
	}
	return &s, nil
}

// Select narrows the set to the named arms, in the order given.
func (s *ArmSet) Select(names []string) error {
	if len(names) == 0 {
		return nil
	}
	byName := map[string]Arm{}
	for _, a := range s.Arms {
		byName[a.Name] = a
	}
	var kept []Arm
	seen := map[string]bool{}
	for _, n := range names {
		a, ok := byName[n]
		if !ok {
			return fmt.Errorf("no arm %q (arms: %s)", n, strings.Join(s.names(), ", "))
		}
		if seen[n] {
			return fmt.Errorf("duplicate arm %q in --arms", n)
		}
		seen[n] = true
		kept = append(kept, a)
	}
	s.Arms = kept
	return nil
}

func (s *ArmSet) names() []string {
	var out []string
	for _, a := range s.Arms {
		out = append(out, a.Name)
	}
	return out
}

func (s *ArmSet) isGuideTool(name string) bool {
	for _, g := range s.GuideTools {
		if g == name {
			return true
		}
	}
	return false
}

// Surface is what one arm shows the model of a connected server.
type Surface struct {
	Arm          Arm
	ServerName   string
	Instructions string // empty unless the arm passes them
	Skill        string // empty unless the arm preloads it
	Tools        []*mcp.Tool
}

// Realize reads the connected server's surface and applies the arm. It errors
// when the arm asks for guidance the server does not offer.
func (s *ArmSet) Realize(ctx context.Context, session *mcp.ClientSession, arm Arm) (*Surface, error) {
	init := session.InitializeResult()
	surf := &Surface{Arm: arm}
	if init != nil && init.ServerInfo != nil {
		surf.ServerName = init.ServerInfo.Name
	}
	if arm.Instructions {
		if init == nil || strings.TrimSpace(init.Instructions) == "" {
			return nil, fmt.Errorf("arm %q passes server instructions, but the server sent none", arm.Name)
		}
		surf.Instructions = init.Instructions
	}

	var all []*mcp.Tool
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		all = append(all, t)
	}
	listed := map[string]bool{}
	for _, t := range all {
		if s.isGuideTool(t.Name) {
			listed[t.Name] = true
			if !arm.Guide {
				continue
			}
		}
		surf.Tools = append(surf.Tools, t)
	}
	if arm.Guide {
		// The whole configured guide surface, or the arm is not what it says.
		var missing []string
		for _, g := range s.GuideTools {
			if !listed[g] {
				missing = append(missing, g)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("arm %q exposes the guide, but the server does not list %s", arm.Name, strings.Join(missing, ", "))
		}
	}
	if len(surf.Tools) == 0 {
		return nil, fmt.Errorf("arm %q: the server lists no tools", arm.Name)
	}

	if arm.Skill {
		text, err := s.skill(ctx, session)
		if err != nil {
			return nil, fmt.Errorf("arm %q: %w", arm.Name, err)
		}
		surf.Skill = text
	}
	return surf, nil
}

func (s *ArmSet) skill(ctx context.Context, session *mcp.ClientSession) (string, error) {
	if s.SkillResource != "" {
		res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: s.SkillResource})
		if err != nil {
			return "", fmt.Errorf("read skill resource %s: %w", s.SkillResource, err)
		}
		var b strings.Builder
		for _, c := range res.Contents {
			b.WriteString(c.Text)
		}
		if strings.TrimSpace(b.String()) == "" {
			return "", fmt.Errorf("skill resource %s is empty", s.SkillResource)
		}
		return b.String(), nil
	}
	path := filepath.Join(s.dir, s.SkillFile)
	// Lexically local is not enough: a symlink beside the arms file could
	// point anywhere, and the skill goes to the model.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		dir, derr := filepath.EvalSymlinks(s.dir)
		if rel, rerr := filepath.Rel(dir, real); derr != nil || rerr != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("skill file %s resolves outside the arms file's directory", s.SkillFile)
		}
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("skill file %s not found beside the arms file: add the skill draft there, or set skill_resource once the server serves it (eval/README.md, multi-turn mode)", s.SkillFile)
	}
	if err != nil {
		return "", fmt.Errorf("skill file: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("skill file %s is empty", s.SkillFile)
	}
	return string(data), nil
}
