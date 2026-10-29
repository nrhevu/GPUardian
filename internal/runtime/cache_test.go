package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type scriptedResolver struct {
	calls    map[string]int
	name     string
	workload KubernetesWorkload
	err      error
}

func (r *scriptedResolver) count(kind string) {
	if r.calls == nil {
		r.calls = make(map[string]int)
	}
	r.calls[kind]++
}

func (r *scriptedResolver) ResolveDockerContainer(context.Context, string) (string, error) {
	r.count("id")
	return "id", r.err
}

func (r *scriptedResolver) DockerContainerName(context.Context, string) (string, error) {
	r.count("name")
	return r.name, r.err
}

func (r *scriptedResolver) NamespaceForContainer(context.Context, string) (string, error) {
	return r.workload.Namespace, r.err
}

func (r *scriptedResolver) KubernetesWorkloadForContainer(context.Context, string) (KubernetesWorkload, error) {
	r.count("workload")
	return r.workload, r.err
}

func TestCachedResolverLifetimes(t *testing.T) {
	complete := KubernetesWorkload{Namespace: "training", PodName: "trainer-0", ContainerName: "worker"}
	dockerName := func(r *CachedResolver) { _, _ = r.DockerContainerName(context.Background(), "abc") }
	dockerID := func(r *CachedResolver) { _, _ = r.ResolveDockerContainer(context.Background(), "trainer") }
	workload := func(r *CachedResolver) { _, _ = r.KubernetesWorkloadForContainer(context.Background(), "abc") }
	tests := []struct {
		name   string
		base   scriptedResolver
		lookup func(*CachedResolver)
		kind   string
		// The result must be reused at cachedFor and looked up again at
		// expiresBy, both measured from the first lookup.
		cachedFor time.Duration
		expiresBy time.Duration
	}{
		{name: "docker name", base: scriptedResolver{name: "trainer"}, lookup: dockerName, kind: "name",
			cachedFor: defaultDockerNameTTL - time.Second, expiresBy: defaultDockerNameTTL},
		{name: "docker name failure", base: scriptedResolver{err: errors.New("docker unavailable")}, lookup: dockerName, kind: "name",
			cachedFor: defaultResolverTTL - time.Second, expiresBy: defaultResolverTTL},
		{name: "docker name to id", base: scriptedResolver{}, lookup: dockerID, kind: "id",
			cachedFor: defaultResolverTTL - time.Second, expiresBy: defaultResolverTTL},
		{name: "complete kubernetes workload", base: scriptedResolver{workload: complete}, lookup: workload, kind: "workload",
			cachedFor: time.Hour, expiresBy: defaultKubernetesTTL},
		{name: "namespace-only kubernetes workload", base: scriptedResolver{workload: KubernetesWorkload{Namespace: "training"}}, lookup: workload, kind: "workload",
			cachedFor: defaultDockerNameTTL - time.Second, expiresBy: defaultDockerNameTTL},
		{name: "kubernetes failure", base: scriptedResolver{err: ErrNotFound}, lookup: workload, kind: "workload",
			cachedFor: defaultResolverTTL - time.Second, expiresBy: defaultResolverTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.base
			start := time.Unix(100, 0)
			now := start
			resolver := NewCachedResolver(&base)
			resolver.now = func() time.Time { return now }
			for _, step := range []struct {
				at        time.Duration
				wantCalls int
			}{{0, 1}, {tc.cachedFor, 1}, {tc.expiresBy, 2}} {
				now = start.Add(step.at)
				tc.lookup(resolver)
				if got := base.calls[tc.kind]; got != step.wantCalls {
					t.Fatalf("after %v: calls = %d, want %d", step.at, got, step.wantCalls)
				}
			}
		})
	}
}

func TestCachedLookupReportsOnlyFreshSuccesses(t *testing.T) {
	base := &scriptedResolver{name: "trainer", workload: KubernetesWorkload{Namespace: "training", PodName: "p", ContainerName: "c"}}
	now := time.Unix(100, 0)
	resolver := NewCachedResolver(base)
	resolver.now = func() time.Time { return now }
	if _, ok := resolver.CachedDockerContainerName("ABC"); ok {
		t.Fatal("cache hit before any lookup")
	}
	if _, err := resolver.DockerContainerName(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.KubernetesWorkloadForContainer(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if name, ok := resolver.CachedDockerContainerName(" ABC "); !ok || name != "trainer" {
		t.Fatalf("cached docker name = %q, %v", name, ok)
	}
	if workload, ok := resolver.CachedKubernetesWorkload("abc"); !ok || workload.PodName != "p" {
		t.Fatalf("cached workload = %+v, %v", workload, ok)
	}
	now = now.Add(defaultDockerNameTTL)
	if _, ok := resolver.CachedDockerContainerName("abc"); ok {
		t.Fatal("expired docker name reported as cached")
	}
	failing := NewCachedResolver(&scriptedResolver{err: errors.New("docker unavailable")})
	_, _ = failing.DockerContainerName(context.Background(), "abc")
	if _, ok := failing.CachedDockerContainerName("abc"); ok {
		t.Fatal("cached failure reported as a cache hit")
	}
}

func TestKubectlPodListIsSharedAcrossContainers(t *testing.T) {
	ids := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	pods := []kubePod{
		{namespace: "training", name: "a-0", containerStatuses: []KubeContainerStatus{{Name: "a", ContainerID: "containerd://" + ids[0]}}},
		{namespace: "training", name: "b-0", initContainerStatuses: []KubeContainerStatus{{Name: "b", ContainerID: "containerd://" + ids[1]}}},
	}
	now := time.Unix(100, 0)
	fetches := 0
	cache := &podListCache{now: func() time.Time { return now }}
	cache.fetch = func(context.Context) ([]kubePod, error) {
		fetches++
		return pods, nil
	}
	resolver := CLIResolver{pods: cache}
	steps := []struct {
		name        string
		advance     time.Duration
		id          string
		wantPod     string
		wantFetches int
	}{
		{name: "first container fetches the listing", id: ids[0], wantPod: "a-0", wantFetches: 1},
		{name: "second container reuses it", id: ids[1], wantPod: "b-0", wantFetches: 1},
		{name: "miss on a fresh listing is definitive", id: ids[2], wantFetches: 1},
		{name: "miss on an older listing refetches once", advance: podListMissRefresh, id: ids[2], wantFetches: 2},
		{name: "hit within TTL", advance: podListTTL - time.Millisecond, id: ids[0], wantPod: "a-0", wantFetches: 2},
		{name: "expired listing is refetched", advance: time.Millisecond, id: ids[0], wantPod: "a-0", wantFetches: 3},
	}
	for _, step := range steps {
		now = now.Add(step.advance)
		workload, err := resolver.workloadFromKubectl(context.Background(), step.id)
		if step.wantPod == "" {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s: err = %v, want not found", step.name, err)
			}
		} else if err != nil || workload.PodName != step.wantPod || workload.Namespace != "training" {
			t.Fatalf("%s: workload = %+v, err = %v", step.name, workload, err)
		}
		if fetches != step.wantFetches {
			t.Fatalf("%s: fetches = %d, want %d", step.name, fetches, step.wantFetches)
		}
	}
}
