package tekton

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"octomaton.dev/internal/services/ci"
)

const (
	defaultWorkers = 4
	defaultResync  = 5 * time.Minute
	// maxRetries bounds the retries of a failing run until it changes again.
	maxRetries = 10
	// reconcileLimit bounds one hand-over.
	reconcileLimit = 2 * time.Minute
)

// liveRuns selects the runs Octomaton has not let go.
const liveRuns = labelManagedBy + "=" + managedByValue + ",!" + labelDone

// Watch hands w every live PipelineRun in every namespace when it changes, when w asks to look again
// and on every resync, until ctx ends. A run deleted before it was let go is handed to w.Deleted.
// Failed hand-overs are retried with backoff, up to maxRetries times.
func (r *Runner) Watch(ctx context.Context, w ci.Watcher) error {
	workers, resync := r.Workers, r.Resync
	if workers <= 0 {
		workers = defaultWorkers
	}
	if resync <= 0 {
		resync = defaultResync
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(r.Dynamic, resync, metav1.NamespaceAll, func(o *metav1.ListOptions) {
		o.LabelSelector = liveRuns
	})
	informer := factory.ForResource(PipelineRuns).Informer()
	queue := workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[string](),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "octomaton-runs"},
	)
	defer queue.ShutDown()
	if _, err := informer.AddEventHandler(r.handler(queue)); err != nil {
		return fmt.Errorf("registering the PipelineRun event handler: %w", err)
	}

	factory.Start(ctx.Done())
	defer factory.Shutdown()
	log := r.logger()
	log.Info("Waiting for the PipelineRun informer to sync")
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return ctx.Err()
	}
	r.synced.Store(true)
	defer r.synced.Store(false)
	log.Info("Watching PipelineRuns", "workers", workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for r.next(ctx, queue, informer.GetIndexer(), w) {
			}
		})
	}
	<-ctx.Done()
	queue.ShutDown()
	wg.Wait()
	log.Info("Stopped watching PipelineRuns")
	return nil
}

// Synced reports whether Watch has read every live run (false when it is not running).
func (r *Runner) Synced() bool { return r.synced.Load() }

func (r *Runner) handler(queue workqueue.TypedRateLimitingInterface[string]) cache.ResourceEventHandlerFuncs {
	enqueue := func(obj any) {
		if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
			queue.Add(key)
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueue,
		UpdateFunc: func(_, obj any) { enqueue(obj) },
		DeleteFunc: func(obj any) {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			// A run labelled done leaves the watch as a deletion too; only a run deleted before it
			// was let go needs attention.
			if pr, ok := obj.(*unstructured.Unstructured); ok && pr.GetLabels()[labelDone] == "" {
				r.rememberDeleted(pr)
				enqueue(pr)
			}
		},
	}
}

func (r *Runner) rememberDeleted(pr *unstructured.Unstructured) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleted == nil {
		r.deleted = map[string]*unstructured.Unstructured{}
	}
	r.deleted[pr.GetNamespace()+"/"+pr.GetName()] = pr
}

func (r *Runner) takeDeleted(key string) *unstructured.Unstructured {
	r.mu.Lock()
	defer r.mu.Unlock()
	pr := r.deleted[key]
	delete(r.deleted, key)
	return pr
}

// next hands over one queued run; it returns false once the queue shuts down.
func (r *Runner) next(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string], indexer cache.Indexer, w ci.Watcher) bool {
	key, shutdown := queue.Get()
	if shutdown {
		return false
	}
	defer queue.Done(key)

	start := time.Now()
	again, err := r.handOver(ctx, key, indexer, w)
	result := "ok"
	switch {
	case err == nil:
		queue.Forget(key)
		if again > 0 {
			queue.AddAfter(key, again)
		}
	case queue.NumRequeues(key) < maxRetries:
		result = "retry"
		r.logger().Warn("Handling a PipelineRun failed; will retry", "key", key, "error", err)
		queue.AddRateLimited(key)
	default:
		result = "error"
		r.logger().Error("Handling a PipelineRun failed; giving up until it changes", "key", key, "error", err)
		queue.Forget(key)
	}
	r.metrics().ReconcileDone(ctx, result, time.Since(start))
	return true
}

func (r *Runner) handOver(ctx context.Context, key string, indexer cache.Indexer, w ci.Watcher) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, reconcileLimit)
	defer cancel()
	obj, exists, err := indexer.GetByKey(key)
	if err != nil {
		return 0, err
	}
	if !exists {
		if pr := r.takeDeleted(key); pr != nil {
			return 0, w.Deleted(ctx, runOf(pr))
		}
		return 0, nil
	}
	pr, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return 0, nil
	}
	return w.Reconcile(ctx, runOf(pr))
}
