package actions

// MarkSourceChanged makes the last counted successful or tolerated run
// incomplete when tracked source changed. It preserves already non-passing
// evidence and returns the changed index, or -1 when no change is needed.
func MarkSourceChanged(steps []StepEvidence) int {
	for index := len(steps) - 1; index >= 0; index-- {
		step := &steps[index]
		if step.Role != RoleRun && step.Role != RoleTolerated || step.Status == StatusSkipped || step.Status == StatusNotRun {
			continue
		}
		if StepConclusion(*step) != ResultSuccess {
			return -1
		}
		step.Status, step.Role = StatusIncomplete, RoleRun
		return index
	}
	return -1
}
