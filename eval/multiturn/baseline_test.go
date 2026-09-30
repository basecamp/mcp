package multiturn

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rec(model, arm, task string, pass bool, score float64, safety int) Record {
	return Record{Model: model, ModelID: model + "-id", Arm: arm, TaskID: task, Pass: pass, Score: score, Safety: safety}
}

func baselineOf(t *testing.T, records ...Record) *Baseline {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, WriteJSONL(&buf, records))
	b, err := LoadBaseline(&buf)
	require.NoError(t, err)
	return b
}

func TestCompareClassifiesEachKind(t *testing.T) {
	base := baselineOf(t,
		rec("haiku", "bare", "a", true, 1, 0),
		rec("haiku", "bare", "b", false, 0.5, 0),
		rec("haiku", "bare", "c", true, 1, 0),
		rec("haiku", "bare", "d", false, 0, 0),
		rec("haiku", "bare", "gone", true, 1, 0),
		rec("haiku", "guide", "a", true, 1, 0),
		rec("haiku", "guide", "e", false, 0, 0),
	)
	cmp, err := Compare(base, []Record{
		rec("haiku", "bare", "a", false, 0.5, 0), // newly failing
		rec("haiku", "bare", "b", false, 0, 0),   // score drop
		rec("haiku", "bare", "c", false, 0, 1),   // safety
		rec("haiku", "bare", "d", true, 1, 0),    // improved
		rec("haiku", "bare", "new", true, 1, 0),  // added
		rec("haiku", "guide", "a", true, 1, 0),   // unchanged
		func() Record { r := rec("haiku", "guide", "e", false, 0, 0); r.Error = "api down"; return r }(), // errored: unmeasured
	})
	require.NoError(t, err)
	kinds := map[string]string{}
	for _, r := range cmp.Regressions {
		kinds[r.TaskID] = r.Kind
	}
	assert.Equal(t, map[string]string{"a": "newly-failing", "b": "score-drop", "c": "safety", "e": "error"}, kinds)
	require.Len(t, cmp.Improved, 1)
	assert.Equal(t, "d", cmp.Improved[0].TaskID)
	assert.Equal(t, []string{"haiku/bare/new"}, cmp.Added)
	assert.Equal(t, []string{"haiku/bare/gone"}, cmp.Removed)
	assert.True(t, cmp.HasRegression())
	out := cmp.Render("base.jsonl")
	assert.Contains(t, out, "REGRESSIONS (4)")
	assert.Contains(t, out, "newly-failing  haiku/bare/a")
}

func TestCompareNoChange(t *testing.T) {
	base := baselineOf(t, rec("haiku", "bare", "a", true, 1, 0))
	cmp, err := Compare(base, []Record{rec("haiku", "bare", "a", true, 1, 0)})
	require.NoError(t, err)
	assert.False(t, cmp.HasRegression())
	assert.Contains(t, cmp.Render("b"), "no change")
}

func TestCompareRefusesNothingToCompareAndModelSwaps(t *testing.T) {
	base := baselineOf(t, rec("haiku", "bare", "a", true, 1, 0))
	_, err := Compare(base, []Record{rec("sonnet", "bare", "a", true, 1, 0)})
	assert.ErrorContains(t, err, "matched no cells")

	swapped := rec("haiku", "bare", "a", true, 1, 0)
	swapped.ModelID = "other"
	_, err = Compare(base, []Record{swapped})
	assert.ErrorContains(t, err, "not like-for-like")

	assert.ErrorContains(t, base.CheckModelIDs(map[string]string{"haiku": "other"}), "not the same model")
	assert.NoError(t, base.CheckModelIDs(map[string]string{"haiku": "haiku-id"}))
	assert.ErrorContains(t, base.CheckOverlap([]string{"haiku"}, []string{"guide"}, []string{"a"}), "shares no")
	assert.NoError(t, base.CheckOverlap([]string{"haiku"}, []string{"bare"}, []string{"a"}))
}

func TestLoadBaselineRejectsBrokenFiles(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteJSONL(&buf, []Record{rec("haiku", "bare", "a", true, 1, 0)}))
	line := strings.TrimSpace(buf.String())

	for name, input := range map[string]string{
		"empty":       "",
		"not json":    "{",
		"missing key": `{"model":"haiku","arm":"bare","task_id":"a"}`,
		"duplicate":   line + "\n" + line,
		"no identity": strings.Replace(line, `"task_id":"a"`, `"task_id":""`, 1),
	} {
		_, err := LoadBaseline(strings.NewReader(input))
		assert.Error(t, err, name)
	}
}
