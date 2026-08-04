package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestCommandReportsNotFoundRequiresBoundedDefinitiveStderr(t *testing.T) {
	if !commandReportsNotFound(&commandError{name: "docker", err: errors.New("exit 1"), stderr: "Error: No such object: abc"}) {
		t.Fatal("definitive docker not-found error was not classified")
	}
	if commandReportsNotFound(&commandError{name: "docker", err: errors.New("exit 1"), stderr: "daemon unavailable"}) {
		t.Fatal("infrastructure error was classified as not found")
	}
	if commandReportsNotFound(&commandError{name: "docker", err: errors.New("exit 1"), stderr: "not found", stderrExceeded: true}) {
		t.Fatal("truncated stderr was treated as definitive")
	}
	if commandReportsNotFound(&commandError{name: "docker", err: errors.New("exit 1"), stderr: `context "prod" not found`}) {
		t.Fatal("missing Docker context was classified as a missing container")
	}
	if !commandReportsNotFound(&commandError{name: "crictl", err: errors.New("exit 1"), stderr: "rpc error: code = NotFound desc = container missing"}) {
		t.Fatal("definitive CRI NotFound status was not classified")
	}
	if commandReportsNotFound(&commandError{name: "crictl", err: errors.New("exit 1"), stderr: "config file not found"}) {
		t.Fatal("missing CRI configuration was classified as a missing container")
	}
}

func TestCombineNamespaceErrorsRequiresAllAvailableResolversToAgree(t *testing.T) {
	transient := errors.New("CRI socket unavailable")
	if err := combineNamespaceErrors(transient, ErrNotFound); errors.Is(err, ErrNotFound) {
		t.Fatalf("transient CRI failure plus kubectl absence became definitive: %v", err)
	}
	if err := combineNamespaceErrors(ErrNotFound, ErrNotFound); !errors.Is(err, ErrNotFound) {
		t.Fatalf("two definitive misses were not classified as absent: %v", err)
	}
	if err := combineNamespaceErrors(exec.ErrNotFound, ErrNotFound); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unavailable CRI plus definitive kubectl miss was not classified as absent: %v", err)
	}
}

func TestFindNamespaceUsesTrustedCRIStatusMetadata(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "status label",
			raw:  `{"status":{"labels":{"io.kubernetes.pod.namespace":"training"}}}`,
			want: "training",
		},
		{
			name: "status annotation",
			raw:  `{"status":{"annotations":{"io.kubernetes.pod.namespace":"inference"}}}`,
			want: "inference",
		},
		{
			name: "empty label falls back to annotation",
			raw:  `{"status":{"labels":{"io.kubernetes.pod.namespace":""},"annotations":{"io.kubernetes.pod.namespace":"batch"}}}`,
			want: "batch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(test.raw), &value); err != nil {
				t.Fatal(err)
			}
			if got := findNamespace(value); got != test.want {
				t.Fatalf("findNamespace() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFindNamespaceRejectsUntrustedNestedFields(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "top-level namespace", raw: `{"namespace":"attacker"}`},
		{name: "top-level labels", raw: `{"labels":{"io.kubernetes.pod.namespace":"attacker"}}`},
		{name: "top-level annotations", raw: `{"annotations":{"io.kubernetes.pod.namespace":"attacker"}}`},
		{name: "status metadata namespace", raw: `{"status":{"metadata":{"namespace":"attacker"}}}`},
		{name: "nested status annotations", raw: `{"status":{"info":{"annotations":{"io.kubernetes.pod.namespace":"attacker"}}}}`},
		{name: "arbitrary recursive namespace", raw: `{"info":{"runtimeSpec":{"namespace":"attacker"}}}`},
		{name: "array nested labels", raw: `{"status":{"children":[{"labels":{"io.kubernetes.pod.namespace":"attacker"}}]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(test.raw), &value); err != nil {
				t.Fatal(err)
			}
			if got := findNamespace(value); got != "" {
				t.Fatalf("findNamespace() trusted adversarial namespace %q", got)
			}
		})
	}
}

func TestFindKubernetesWorkloadUsesTrustedCRIStatusMetadata(t *testing.T) {
	var value any
	if err := json.Unmarshal([]byte(`{
		"status": {
			"metadata": {"name": "worker"},
			"labels": {
				"io.kubernetes.pod.namespace": "training",
				"io.kubernetes.pod.name": "trainer-0",
				"io.kubernetes.container.name": "trainer"
			}
		}
	}`), &value); err != nil {
		t.Fatal(err)
	}
	got := findKubernetesWorkload(value)
	want := (KubernetesWorkload{Namespace: "training", PodName: "trainer-0", ContainerName: "trainer"})
	if got != want {
		t.Fatalf("workload = %+v, want %+v", got, want)
	}
}

func TestMatchingContainerNameIncludesInitContainers(t *testing.T) {
	full := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	statuses := []KubeContainerStatus{{Name: "setup", ContainerID: "containerd://" + full}}
	name, ok := matchingContainerName(statuses, full, shortID(full))
	if !ok || name != "setup" {
		t.Fatalf("matching container = %q, %v", name, ok)
	}
}

type countingResolver struct {
	dockerNameCalls int
	onDockerName    func()
}

func (r *countingResolver) ResolveDockerContainer(context.Context, string) (string, error) {
	return "id", nil
}

func (r *countingResolver) DockerContainerName(context.Context, string) (string, error) {
	r.dockerNameCalls++
	if r.onDockerName != nil {
		r.onDockerName()
	}
	return "trainer", nil
}

type blockingResolver struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

type countingWorkloadResolver struct {
	calls int
}

func (r *countingWorkloadResolver) ResolveDockerContainer(context.Context, string) (string, error) {
	return "id", nil
}

func (r *countingWorkloadResolver) DockerContainerName(context.Context, string) (string, error) {
	return "trainer", nil
}

func (r *countingWorkloadResolver) NamespaceForContainer(context.Context, string) (string, error) {
	return "training", nil
}

func (r *countingWorkloadResolver) KubernetesWorkloadForContainer(context.Context, string) (KubernetesWorkload, error) {
	r.calls++
	return KubernetesWorkload{Namespace: "training", PodName: "trainer-0", ContainerName: "worker"}, nil
}

func (r *blockingResolver) ResolveDockerContainer(context.Context, string) (string, error) {
	return "id", nil
}

func (r *blockingResolver) DockerContainerName(context.Context, string) (string, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.release
	return "trainer", nil
}

func (r *blockingResolver) NamespaceForContainer(context.Context, string) (string, error) {
	return "training", nil
}

func (r *countingResolver) NamespaceForContainer(context.Context, string) (string, error) {
	return "training", nil
}

func TestCachedResolverExpiresFromLookupCompletion(t *testing.T) {
	now := time.Unix(100, 0)
	base := &countingResolver{onDockerName: func() { now = now.Add(10 * time.Second) }}
	resolver := NewCachedResolver(base)
	resolver.TTL = time.Second
	resolver.now = func() time.Time { return now }
	if _, err := resolver.DockerContainerName(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.DockerContainerName(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if base.dockerNameCalls != 1 {
		t.Fatalf("slow lookup was expired at insertion: calls=%d", base.dockerNameCalls)
	}
}

func TestCachedResolverCoalescesConcurrentLookup(t *testing.T) {
	base := &blockingResolver{started: make(chan struct{}, 1), release: make(chan struct{})}
	resolver := NewCachedResolver(base)
	const workers = 16
	start := make(chan struct{})
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := resolver.DockerContainerName(context.Background(), "abc")
			errCh <- err
		}()
	}
	close(start)
	<-base.started
	close(base.release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	base.mu.Lock()
	calls := base.calls
	base.mu.Unlock()
	if calls != 1 {
		t.Fatalf("concurrent runtime lookups = %d, want 1", calls)
	}
}

func TestCachedResolverReusesAndExpiresLookup(t *testing.T) {
	base := &countingResolver{}
	now := time.Unix(100, 0)
	resolver := NewCachedResolver(base)
	resolver.TTL = time.Second
	resolver.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if _, err := resolver.DockerContainerName(context.Background(), "ABC"); err != nil {
			t.Fatal(err)
		}
	}
	if base.dockerNameCalls != 1 {
		t.Fatalf("runtime lookups = %d, want 1", base.dockerNameCalls)
	}
	now = now.Add(time.Second)
	if _, err := resolver.DockerContainerName(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if base.dockerNameCalls != 2 {
		t.Fatalf("runtime lookups after expiry = %d, want 2", base.dockerNameCalls)
	}
}

func TestCachedResolverReusesFullKubernetesWorkload(t *testing.T) {
	base := &countingWorkloadResolver{}
	resolver := NewCachedResolver(base)
	for i := 0; i < 2; i++ {
		workload, err := resolver.KubernetesWorkloadForContainer(context.Background(), "ABC")
		if err != nil {
			t.Fatal(err)
		}
		if workload.PodName != "trainer-0" || workload.ContainerName != "worker" {
			t.Fatalf("workload = %+v", workload)
		}
	}
	if base.calls != 1 {
		t.Fatalf("runtime workload lookups = %d, want 1", base.calls)
	}
}
