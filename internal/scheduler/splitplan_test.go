package scheduler

import (
	"strings"
	"testing"

	"github.com/prathyushnallamothu/potluck/internal/registry"
)

const gib = uint64(1) << 30

// splitNode builds a node with the given free memory and an optional model
// of the given size.
func splitNode(name string, freeMem uint64, modelSize uint64, worker, driver bool) registry.NodeState {
	n := node(name, nil)
	n.Resources.MemAvailable = freeMem
	n.Resources.MemTotal = freeMem * 2
	n.Split.Worker = worker
	n.Split.Driver = driver
	if modelSize > 0 {
		n.Models = []registry.Model{{Name: "big:latest", Size: modelSize}}
	}
	return n
}

func TestPlanSplitNotNeededWhenFitsSingle(t *testing.T) {
	// 4 GiB model needs ~5 GiB; node has 16 GiB free (12.8 usable).
	n := splitNode("a", 16*gib, 4*gib, true, true)
	plan, err := PlanSplit([]registry.NodeState{n}, "big")
	if err != nil || plan != nil {
		t.Fatalf("want (nil, nil) when model fits a single node, got (%v, %v)", plan, err)
	}
}

func TestPlanSplitUnknownModelIsNotAnError(t *testing.T) {
	n := splitNode("a", 16*gib, 4*gib, true, true)
	plan, err := PlanSplit([]registry.NodeState{n}, "nope")
	if err != nil || plan != nil {
		t.Fatalf("unknown model should defer to Pick's 404, got (%v, %v)", plan, err)
	}
}

func TestPlanSplitRecruitsWorkers(t *testing.T) {
	// 40 GiB model needs 50 GiB. Driver has 16 GiB free (12.8 usable);
	// workers add 32 GiB free each (25.6 usable). Two nodes are enough
	// (12.8 + 25.6 = 38.4 < 50), so it must recruit both workers.
	driver := splitNode("driver", 16*gib, 40*gib, false, true)
	w1 := splitNode("w1", 32*gib, 0, true, false)
	w2 := splitNode("w2", 32*gib, 0, true, false)

	plan, err := PlanSplit([]registry.NodeState{driver, w1, w2}, "big")
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("expected a split plan")
	}
	if plan.Driver.Name != "driver" {
		t.Errorf("driver = %q", plan.Driver.Name)
	}
	if len(plan.Workers) != 2 {
		t.Errorf("recruited %d workers, want 2", len(plan.Workers))
	}
}

func TestPlanSplitStopsRecruitingWhenEnough(t *testing.T) {
	// 10 GiB model needs 12.5 GiB. Driver usable 8 GiB + first worker
	// usable 25.6 GiB covers it; second worker should not be recruited.
	driver := splitNode("driver", 10*gib, 10*gib, false, true)
	w1 := splitNode("w1", 32*gib, 0, true, false)
	w2 := splitNode("w2", 32*gib, 0, true, false)

	plan, err := PlanSplit([]registry.NodeState{driver, w1, w2}, "big")
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || len(plan.Workers) != 1 {
		t.Fatalf("want exactly 1 worker, got %+v", plan)
	}
}

func TestPlanSplitNoDriver(t *testing.T) {
	holder := splitNode("holder", 8*gib, 40*gib, true, false) // no llama-server
	w := splitNode("w", 64*gib, 0, true, false)
	_, err := PlanSplit([]registry.NodeState{holder, w}, "big")
	if err == nil || !strings.Contains(err.Error(), "llama-server") {
		t.Fatalf("want no-driver error, got %v", err)
	}
}

func TestPlanSplitPoolTooSmall(t *testing.T) {
	driver := splitNode("driver", 8*gib, 100*gib, false, true)
	w := splitNode("w", 8*gib, 0, true, false)
	_, err := PlanSplit([]registry.NodeState{driver, w}, "big")
	if err == nil || !strings.Contains(err.Error(), "cannot fit") {
		t.Fatalf("want pool-too-small error, got %v", err)
	}
}
