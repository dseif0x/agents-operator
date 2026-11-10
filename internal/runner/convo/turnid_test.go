package convo

import (
	"strings"
	"testing"
)

// Turn ids differ between runner processes: the counter restarts, the run id
// does not repeat, so a new turn never updates an earlier process's items.
func TestTurnIDsCarryRunID(t *testing.T) {
	id := newTurnID("a", 1)
	if !strings.HasPrefix(id, "a"+runID+"-") || id == "a1" {
		t.Fatalf("turn id %q lacks the run id %q", id, runID)
	}
	if len(runID) != 6 {
		t.Fatalf("run id %q", runID)
	}
}
