package tekton

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"octomaton.dev/internal/services/ci"
)

const (
	// annotationReruns holds how a run reruns after disruptions (ci.Reruns, JSON).
	annotationReruns = "octomaton.dev/reruns"
	// disruptedPrefix starts the annotation that records, one per pod, a pod of the run that the
	// cluster took away: disrupted.octomaton.dev/<pod> holds its ci.Disruption (JSON). A key of its
	// own per pod lets each be written without reading the others; Tekton keeps pod names within 63
	// characters, an annotation name's limit.
	disruptedPrefix = "disrupted.octomaton.dev/"
	// labelPipelineRun names the PipelineRun a TaskRun or pod belongs to; Tekton sets it.
	labelPipelineRun = "tekton.dev/pipelineRun"
	// podsSyncLimit bounds the wait for a namespace's pods to be read the first time.
	podsSyncLimit = 30 * time.Second
	// markLimit bounds recording one disrupted pod on its run.
	markLimit = 10 * time.Second
	// rememberedFor is how long a disrupted pod is remembered after it was seen: by then its run
	// is long over, and its record is on the run.
	rememberedFor = 24 * time.Hour
)

// disruptionOf returns how the cluster took a pod away, when its DisruptionTarget condition says it
// did: the kubelet stopped it for a node shutdown or node pressure, it was evicted, preempted, or
// removed from a node that is gone. A pod that fails by itself, or that the kubelet evicts for
// exceeding its own limits, has no such condition.
func disruptionOf(pod *corev1.Pod) (ci.Disruption, bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue {
			return ci.Disruption{
				Task: pod.Labels[labelPipelineTask], Node: pod.Spec.NodeName, Reason: c.Reason, Message: redactCredentials(c.Message),
			}, true
		}
	}
	return ci.Disruption{}, false
}

// podWatch watches the pods of live runs with reruns, a namespace at a time, and records each pod
// the cluster takes away on its run as it happens: the pod is often deleted soon after its machine
// goes, and a TaskRun keeps no trace of why its pod went. Only the leader runs it, within Watch.
type podWatch struct {
	runner *Runner
	ctx    context.Context
	// runs holds the live runs, as Watch reads them.
	runs cache.Indexer

	mu        sync.Mutex
	factories map[string]informers.SharedInformerFactory
	indexers  map[string]cache.Indexer
	// seen remembers the disrupted pods seen, by namespace/pod, until their record is on their run.
	seen map[string]seenPod
}

type seenPod struct {
	disruption ci.Disruption
	at         time.Time
}

func newPodWatch(ctx context.Context, r *Runner, runs cache.Indexer) *podWatch {
	return &podWatch{
		runner: r, ctx: ctx, runs: runs,
		factories: map[string]informers.SharedInformerFactory{}, indexers: map[string]cache.Indexer{}, seen: map[string]seenPod{},
	}
}

// watch starts watching a namespace's pods unless it does already, and returns once it read them
// (or podsSyncLimit passed), so that the disrupted pods there are known.
func (d *podWatch) watch(namespace string) {
	d.mu.Lock()
	if _, ok := d.factories[namespace]; ok {
		d.mu.Unlock()
		return
	}
	factory := informers.NewSharedInformerFactoryWithOptions(d.runner.Kube, 0, informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = labelPipelineRun }),
		informers.WithTransform(slimPod))
	informer := factory.Core().V1().Pods().Informer()
	reg, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { d.saw(obj) },
		UpdateFunc: func(_, obj any) { d.saw(obj) },
		DeleteFunc: func(obj any) {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			d.saw(obj)
		},
	})
	if err != nil {
		d.mu.Unlock()
		d.runner.logger().Error("Could not watch a namespace's pods", "namespace", namespace, "error", err)
		return
	}
	d.factories[namespace], d.indexers[namespace] = factory, informer.GetIndexer()
	d.mu.Unlock()
	factory.Start(d.ctx.Done())
	ctx, cancel := context.WithTimeout(d.ctx, podsSyncLimit)
	defer cancel()
	if !cache.WaitForCacheSync(ctx.Done(), reg.HasSynced) {
		d.runner.logger().Warn("Pods of a namespace not read in time; disruptions there may be missed until they are", "namespace", namespace)
		return
	}
	d.runner.logger().Info("Watching pods for disruptions", "namespace", namespace)
}

// stop stops watching pods, once Watch's context ended.
func (d *podWatch) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.factories {
		f.Shutdown()
	}
}

// slimPod keeps of a pod only what podWatch reads, so that a namespace's pods take little memory.
func slimPod(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: pod.Name, Namespace: pod.Namespace, ResourceVersion: pod.ResourceVersion, Labels: pod.Labels,
		},
		Spec:   corev1.PodSpec{NodeName: pod.Spec.NodeName},
		Status: corev1.PodStatus{Conditions: pod.Status.Conditions},
	}, nil
}

// saw records a pod the cluster took away on its run, when that run is live, has reruns and does not
// record it yet. A record that fails is logged; the pod's next change, or a new leader's first look,
// tries again, and Details reads it from memory meanwhile.
func (d *podWatch) saw(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	disruption, ok := disruptionOf(pod)
	if !ok {
		return
	}
	item, exists, err := d.runs.GetByKey(pod.Namespace + "/" + pod.Labels[labelPipelineRun])
	if err != nil || !exists {
		return
	}
	pr, ok := item.(*unstructured.Unstructured)
	if !ok || pr.GetAnnotations()[annotationReruns] == "" {
		return
	}
	d.remember(pod, disruption)
	key := disruptedPrefix + pod.Name
	if pr.GetAnnotations()[key] != "" {
		return
	}
	data, err := json.Marshal(disruption)
	if err != nil {
		return
	}
	value := string(data)
	ctx, cancel := context.WithTimeout(d.ctx, markLimit)
	defer cancel()
	log := d.runner.logger().With("run", pr.GetNamespace()+"/"+pr.GetName(), "pod", pod.Name, "task", disruption.Task, "node", disruption.Node, "reason", disruption.Reason)
	if err := d.runner.client().Annotate(ctx, pr.GetNamespace(), pr.GetName(), map[string]*string{key: &value}); err != nil {
		log.Warn("Could not record a disrupted pod on its run", "error", err)
		return
	}
	log.Info("A run's pod was disrupted")
}

func (d *podWatch) remember(pod *corev1.Pod, disruption ci.Disruption) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, s := range d.seen {
		if now.Sub(s.at) > rememberedFor {
			delete(d.seen, k)
		}
	}
	key := pod.Namespace + "/" + pod.Name
	if _, ok := d.seen[key]; !ok {
		d.seen[key] = seenPod{disruption: disruption, at: now}
	}
}

// pod returns how the cluster took a pod away, when this leader saw it.
func (d *podWatch) pod(namespace, name string) (ci.Disruption, bool) {
	d.mu.Lock()
	seen, ok := d.seen[namespace+"/"+name]
	pods := d.indexers[namespace]
	d.mu.Unlock()
	if ok {
		return seen.disruption, true
	}
	if pods == nil {
		return ci.Disruption{}, false
	}
	item, exists, err := pods.GetByKey(namespace + "/" + name)
	if err != nil || !exists {
		return ci.Disruption{}, false
	}
	pod, ok := item.(*corev1.Pod)
	if !ok {
		return ci.Disruption{}, false
	}
	return disruptionOf(pod)
}

// disruptionLookup returns how the cluster took away a pod of a run with reruns: as its run records
// it, or as the leader saw it. It finds nothing for a run without reruns.
func (r *Runner) disruptionLookup(pr *unstructured.Unstructured) func(pod string) *ci.Disruption {
	a := pr.GetAnnotations()
	if a[annotationReruns] == "" {
		return func(string) *ci.Disruption { return nil }
	}
	watcher := r.podWatch.Load()
	return func(pod string) *ci.Disruption {
		if pod == "" {
			return nil
		}
		if raw := a[disruptedPrefix+pod]; raw != "" {
			var d ci.Disruption
			if err := json.Unmarshal([]byte(raw), &d); err == nil {
				return &d
			}
		}
		if watcher == nil {
			return nil
		}
		if d, ok := watcher.pod(pr.GetNamespace(), pod); ok {
			return &d
		}
		return nil
	}
}

// wantsDisruptions reports whether a run's pods are watched for disruptions: it reruns after them,
// and Octomaton has not let it go.
func wantsDisruptions(pr *unstructured.Unstructured) bool {
	return pr.GetAnnotations()[annotationReruns] != "" && pr.GetLabels()[labelDone] == ""
}

// rerunsOf reads how a run reruns after disruptions.
func rerunsOf(raw string) *ci.Reruns {
	if raw == "" {
		return nil
	}
	var r ci.Reruns
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil
	}
	return &r
}
