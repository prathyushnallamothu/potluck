package scheduler

import (
	"fmt"
	"sort"

	"github.com/prathyushnallamothu/potluck/internal/registry"
	"github.com/prathyushnallamothu/potluck/internal/split"
)

// Plan describes how to run a model that fits on no single node: one driver
// (which has the weights and runs llama-server) plus worker nodes recruited
// for their memory.
type Plan struct {
	Driver    registry.NodeState
	Workers   []registry.NodeState
	ModelSize uint64
	Need      uint64
}

// PlanSplit decides whether the model needs splitting and, if so, how.
// Returns:
//
//	(nil, nil)  — splitting not needed (some node fits it whole, or nobody
//	              has the model at all); use Pick instead
//	(plan, nil) — split across plan.Driver + plan.Workers
//	(nil, err)  — splitting is needed but impossible; err says why
func PlanSplit(nodes []registry.NodeState, model string) (*Plan, error) {
	var holders []registry.NodeState
	var size uint64
	for _, n := range nodes {
		if !eligible(n) || !n.HasModel(model) {
			continue
		}
		holders = append(holders, n)
		for _, m := range n.Models {
			if (m.Name == model || m.Name == model+":latest" || m.Name+":latest" == model) && m.Size > size {
				size = m.Size
			}
		}
	}
	if len(holders) == 0 || size == 0 {
		return nil, nil // unknown model: Pick will produce the 404
	}

	need := split.Need(size)
	for _, n := range holders {
		if split.Usable(n.Resources.MemAvailable) >= need {
			return nil, nil // fits whole somewhere; Phase 1 handles it
		}
	}

	// Split required. The driver must hold the weights and run llama-server.
	var drivers []registry.NodeState
	for _, n := range holders {
		if n.Split.Driver {
			drivers = append(drivers, n)
		}
	}
	if len(drivers) == 0 {
		return nil, fmt.Errorf(
			"model %q (%d GiB) fits no single node and no holder has llama-server installed to drive a split",
			model, size>>30)
	}
	sort.Slice(drivers, func(i, j int) bool {
		return drivers[i].Resources.MemAvailable > drivers[j].Resources.MemAvailable
	})
	driver := drivers[0]
	capacity := split.Usable(driver.Resources.MemAvailable)

	// Recruit workers by free memory until the plan fits.
	var candidates []registry.NodeState
	for _, n := range nodes {
		if eligible(n) && n.Split.Worker && n.ID != driver.ID {
			candidates = append(candidates, n)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Resources.MemAvailable > candidates[j].Resources.MemAvailable
	})
	var workers []registry.NodeState
	for _, n := range candidates {
		if capacity >= need {
			break
		}
		workers = append(workers, n)
		capacity += split.Usable(n.Resources.MemAvailable)
	}
	if capacity < need {
		return nil, fmt.Errorf(
			"pool cannot fit model %q: needs ~%d GiB, pool has ~%d GiB usable across driver %q and %d worker(s)",
			model, need>>30, capacity>>30, driver.Name, len(workers))
	}
	return &Plan{Driver: driver, Workers: workers, ModelSize: size, Need: need}, nil
}
