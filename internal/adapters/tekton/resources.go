package tekton

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// finishedRuns selects the runs let go whose PVCs are not freed yet.
const finishedRuns = LabelManagedBy + "=" + ManagedByValue + "," + LabelDone + ",!" + LabelPVCsFreed

// FreeResources deletes the PVCs owned by runs that finished before the given time, and labels
// those runs so they are not examined again. Pods are kept: the Tekton Dashboard reads logs from
// them, and Tekton's pruner deletes old runs with their pods.
func (r *Runner) FreeResources(ctx context.Context, finishedBefore time.Time) (int, error) {
	c := r.client()
	runs, err := c.List(ctx, "", finishedRuns)
	if err != nil {
		return 0, err
	}
	owned := map[string]map[types.UID]string{} // namespace → run UID → run name
	for i := range runs {
		run := &runs[i]
		st, err := GetPipelineRunStatus(run)
		if err != nil || st.CompletionTime == nil || !st.CompletionTime.Time.Before(finishedBefore) {
			continue
		}
		if owned[run.GetNamespace()] == nil {
			owned[run.GetNamespace()] = map[types.UID]string{}
		}
		owned[run.GetNamespace()][run.GetUID()] = run.GetName()
	}
	var errs []error
	deleted := 0
	for ns, byUID := range owned {
		n, err := r.freeNamespace(ctx, ns, byUID)
		deleted += n
		errs = append(errs, err)
	}
	return deleted, errors.Join(errs...)
}

// freeNamespace deletes the PVCs of one namespace's finished runs, then labels the runs, unless a
// deletion failed: the next pass tries the namespace again.
func (r *Runner) freeNamespace(ctx context.Context, ns string, byUID map[types.UID]string) (int, error) {
	c := r.client()
	pvcs, err := c.PVCs(ctx, ns)
	if err != nil {
		return 0, err
	}
	var errs []error
	deleted := 0
	for _, pvc := range pvcs {
		for _, ref := range pvc.OwnerReferences {
			if ref.Kind != KindPipelineRun || byUID[ref.UID] == "" {
				continue
			}
			if err := c.DeletePVC(ctx, ns, pvc.Name); err != nil {
				errs = append(errs, err)
			} else {
				deleted++
			}
			break
		}
	}
	if len(errs) > 0 {
		return deleted, errors.Join(errs...)
	}
	for _, name := range byUID {
		if err := c.Label(ctx, ns, name, map[string]string{LabelPVCsFreed: "true"}, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return deleted, errors.Join(errs...)
}
