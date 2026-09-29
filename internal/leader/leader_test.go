package leader

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"octomaton.dev/internal/metrics/metricstest"
)

func newElector(t *testing.T, client *kubefake.Clientset, jobs func(context.Context)) (*Elector, *metricstest.Metrics) {
	t.Helper()
	m := metricstest.New(t)
	return &Elector{
		Client:    client,
		Namespace: "octomaton",
		Name:      "octomaton",
		Pod:       "octomaton-0",
		Jobs:      jobs,
		Metrics:   m.Metrics,
		Logger:    slog.New(slog.DiscardHandler),
	}, m
}

// eventually fails the test unless cond holds within a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name string
		// holder holds the Lease, freshly renewed, before the election starts.
		holder      string
		wantLeading bool
	}{
		{name: "free Lease", wantLeading: true},
		{name: "Lease held by another replica", holder: "octomaton-1_00000000", wantLeading: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := kubefake.NewClientset()
			if tt.holder != "" {
				now := metav1.NewMicroTime(time.Now())
				_, err := client.CoordinationV1().Leases("octomaton").Create(context.Background(), &coordinationv1.Lease{
					ObjectMeta: metav1.ObjectMeta{Name: "octomaton", Namespace: "octomaton"},
					Spec: coordinationv1.LeaseSpec{
						HolderIdentity: &tt.holder, LeaseDurationSeconds: new(int32(60)), AcquireTime: &now, RenewTime: &now,
					},
				}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			jobsStarted := make(chan struct{})
			e, m := newElector(t, client, func(ctx context.Context) { close(jobsStarted); <-ctx.Done() })
			ctx, cancel := context.WithCancel(context.Background())
			stopped := make(chan struct{})
			go func() { e.Run(ctx); close(stopped) }()

			if tt.wantLeading {
				eventually(t, "the jobs start", func() bool {
					select {
					case <-jobsStarted:
						return true
					default:
						return false
					}
				})
				lease, err := client.CoordinationV1().Leases("octomaton").Get(context.Background(), "octomaton", metav1.GetOptions{})
				if err != nil || lease.Spec.HolderIdentity == nil || !strings.HasPrefix(*lease.Spec.HolderIdentity, "octomaton-0_") {
					t.Fatalf("Lease = %+v, %v; want it held by octomaton-0_*", lease, err)
				}
			} else {
				eventually(t, "the Lease is read", func() bool { return len(client.Actions()) > 1 })
			}
			if e.Leading() != tt.wantLeading || m.Gauge(t, "octomaton.leader") != map[bool]int64{true: 1}[tt.wantLeading] {
				t.Fatalf("Leading() = %v, octomaton.leader = %d; want leading %v", e.Leading(), m.Gauge(t, "octomaton.leader"), tt.wantLeading)
			}

			cancel()
			eventually(t, "Run returns", func() bool {
				select {
				case <-stopped:
					return true
				default:
					return false
				}
			})
			if e.Leading() || m.Gauge(t, "octomaton.leader") != 0 {
				t.Fatalf("after Run: Leading() = %v, octomaton.leader = %d", e.Leading(), m.Gauge(t, "octomaton.leader"))
			}
		})
	}
}

func TestLeadAndStepDown(t *testing.T) {
	live := context.Background()
	lost, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name        string
		steps       func(e *Elector)
		wantLeading bool
		wantJobs    int
	}{
		{name: "lead", steps: func(e *Elector) { e.lead(live, "id") }, wantLeading: true, wantJobs: 1},
		{name: "lead then step down", steps: func(e *Elector) { e.lead(live, "id"); e.stepDown("id") }, wantJobs: 1},
		// client-go stepped down before the goroutine starting the jobs ran.
		{name: "late start after step down", steps: func(e *Elector) { e.stepDown("id"); e.lead(lost, "id") }},
		{name: "step down without leading", steps: func(e *Elector) { e.stepDown("id") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobs := 0
			e, m := newElector(t, kubefake.NewClientset(), func(context.Context) { jobs++ })
			tt.steps(e)
			wantGauge := map[bool]int64{true: 1}[tt.wantLeading]
			if e.Leading() != tt.wantLeading || jobs != tt.wantJobs || m.Gauge(t, "octomaton.leader") != wantGauge {
				t.Fatalf("Leading() = %v, jobs = %d, octomaton.leader = %d; want %v, %d, %d",
					e.Leading(), jobs, m.Gauge(t, "octomaton.leader"), tt.wantLeading, tt.wantJobs, wantGauge)
			}
		})
	}
}

func TestIdentity(t *testing.T) {
	a, b := identity("octomaton-0"), identity("octomaton-0")
	if !strings.HasPrefix(a, "octomaton-0_") || len(a) != len("octomaton-0_")+8 || a == b {
		t.Fatalf("identities %q and %q: want octomaton-0_ and 8 distinct hex digits", a, b)
	}
}
