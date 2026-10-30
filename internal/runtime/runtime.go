package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Resolver interface {
	ResolveDockerContainer(ctx context.Context, nameOrID string) (string, error)
	DockerContainerName(ctx context.Context, containerID string) (string, error)
	NamespaceForContainer(ctx context.Context, containerID string) (string, error)
}

type KubernetesWorkload struct {
	Namespace     string
	PodName       string
	ContainerName string
}

type KubernetesWorkloadResolver interface {
	KubernetesWorkloadForContainer(ctx context.Context, containerID string) (KubernetesWorkload, error)
}

// CachedLookup is implemented by resolvers that can answer from cache alone,
// without running docker, crictl, or kubectl. Only unexpired successful
// lookups are reported.
type CachedLookup interface {
	CachedDockerContainerName(containerID string) (string, bool)
	CachedKubernetesWorkload(containerID string) (KubernetesWorkload, bool)
}

// DockerContainer is one container known to the Docker daemon, running or
// stopped.
type DockerContainer struct {
	ID    string
	Names []string
}

// DockerContainerLister lists every Docker container on the host in one call.
type DockerContainerLister interface {
	DockerContainers(ctx context.Context) ([]DockerContainer, error)
}

var ErrNotFound = errors.New("runtime object not found")

type CLIResolver struct {
	Timeout time.Duration
	// pods shares one kubectl pod listing across container lookups. It is set
	// by NewCLIResolver; a literal CLIResolver lists every pod per lookup.
	pods *podListCache
}

func NewCLIResolver() CLIResolver {
	return CLIResolver{pods: &podListCache{}}
}

const (
	// podListTTL bounds how long one kubectl pod listing answers lookups.
	podListTTL = 5 * time.Second
	// podListMissRefresh is the age after which a lookup that misses in the
	// shared listing fetches a new one, so a just-started container is never
	// declared absent based on a listing older than this.
	podListMissRefresh = time.Second
)

type kubePod struct {
	namespace             string
	name                  string
	containerStatuses     []KubeContainerStatus
	initContainerStatuses []KubeContainerStatus
}

type podListCache struct {
	mu        sync.Mutex
	pods      []kubePod
	fetchedAt time.Time
	call      *podListCall
	now       func() time.Time
	// fetch replaces the kubectl listing in tests.
	fetch func(context.Context) ([]kubePod, error)
}

type podListCall struct {
	done chan struct{}
	pods []kubePod
	at   time.Time
	err  error
}

type cacheEntry struct {
	value     string
	err       error
	expiresAt time.Time
}

type resolveCall struct {
	done  chan struct{}
	value string
	err   error
}

type workloadCacheEntry struct {
	value     KubernetesWorkload
	err       error
	expiresAt time.Time
}

type workloadResolveCall struct {
	done  chan struct{}
	value KubernetesWorkload
	err   error
}

// CachedResolver caches lookups from Base and coalesces concurrent lookups of
// the same key. Unless TTL overrides every lifetime, successful lookups live
// for a lifetime suited to how stable the answer is: a Kubernetes namespace,
// pod, and container name never change for a container ID (IDs are never
// reused), Docker names change only on rename, and a name-to-ID resolution
// must follow container replacement. Failures are retried after
// defaultResolverTTL.
type CachedResolver struct {
	Base Resolver
	TTL  time.Duration

	mu              sync.Mutex
	dockerIDs       map[string]cacheEntry
	dockerNames     map[string]cacheEntry
	dockerIDCalls   map[string]*resolveCall
	dockerNameCalls map[string]*resolveCall
	workloads       map[string]workloadCacheEntry
	workloadCalls   map[string]*workloadResolveCall
	now             func() time.Time
}

const (
	maxResolverCacheEntries = 1024
	maxRuntimeOutputBytes   = 64 << 20
	maxRuntimeErrorBytes    = 64 << 10

	// defaultResolverTTL applies to failures and to Docker name-to-ID
	// resolution.
	defaultResolverTTL = 5 * time.Second
	// defaultDockerNameTTL applies to Docker container names and to partial
	// Kubernetes workloads (namespace without pod or container name).
	defaultDockerNameTTL = 30 * time.Second
	// defaultKubernetesTTL keeps a complete Kubernetes workload for the life
	// of its container in practice; the bounded cache evicts it earlier when
	// full.
	defaultKubernetesTTL = 24 * time.Hour
)

type commandError struct {
	name           string
	err            error
	stderr         string
	stderrExceeded bool
}

func (e *commandError) Error() string { return fmt.Sprintf("%s command failed: %v", e.name, e.err) }
func (e *commandError) Unwrap() error { return e.err }

type boundedOutput struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.Buffer.Write(data)
	}
	if original > remaining {
		b.exceeded = true
	}
	return original, nil
}

func NewCachedResolver(base Resolver) *CachedResolver {
	return &CachedResolver{
		Base:      base,
		dockerIDs: make(map[string]cacheEntry), dockerNames: make(map[string]cacheEntry),
		dockerIDCalls: make(map[string]*resolveCall), dockerNameCalls: make(map[string]*resolveCall),
		workloads: make(map[string]workloadCacheEntry), workloadCalls: make(map[string]*workloadResolveCall),
		now: time.Now,
	}
}

func (r *CachedResolver) ResolveDockerContainer(ctx context.Context, nameOrID string) (string, error) {
	key := strings.TrimSpace(nameOrID)
	return r.resolve(ctx, r.dockerIDs, r.dockerIDCalls, key, r.successTTL(defaultResolverTTL), func() (string, error) {
		return r.Base.ResolveDockerContainer(ctx, nameOrID)
	})
}

func (r *CachedResolver) DockerContainerName(ctx context.Context, containerID string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(containerID))
	return r.resolve(ctx, r.dockerNames, r.dockerNameCalls, key, r.successTTL(defaultDockerNameTTL), func() (string, error) {
		return r.Base.DockerContainerName(ctx, containerID)
	})
}

// DockerContainers is never cached: callers use it to decide that a container
// is gone, which a stale listing must not claim.
func (r *CachedResolver) DockerContainers(ctx context.Context) ([]DockerContainer, error) {
	lister, ok := r.Base.(DockerContainerLister)
	if !ok {
		return nil, errors.ErrUnsupported
	}
	return lister.DockerContainers(ctx)
}

func (r *CachedResolver) CachedDockerContainerName(containerID string) (string, bool) {
	if r == nil {
		return "", false
	}
	key := strings.ToLower(strings.TrimSpace(containerID))
	now := r.nowTime()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, found := r.dockerNames[key]
	if !found || entry.err != nil || !now.Before(entry.expiresAt) {
		return "", false
	}
	return entry.value, true
}

func (r *CachedResolver) CachedKubernetesWorkload(containerID string) (KubernetesWorkload, bool) {
	if r == nil {
		return KubernetesWorkload{}, false
	}
	key := strings.ToLower(strings.TrimSpace(containerID))
	now := r.nowTime()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, found := r.workloads[key]
	if !found || entry.err != nil || !now.Before(entry.expiresAt) {
		return KubernetesWorkload{}, false
	}
	return entry.value, true
}

func (r *CachedResolver) NamespaceForContainer(ctx context.Context, containerID string) (string, error) {
	workload, err := r.KubernetesWorkloadForContainer(ctx, containerID)
	return workload.Namespace, err
}

func (r *CachedResolver) KubernetesWorkloadForContainer(ctx context.Context, containerID string) (KubernetesWorkload, error) {
	if r == nil || r.Base == nil {
		return KubernetesWorkload{}, errors.New("runtime resolver is unavailable")
	}
	key := strings.ToLower(strings.TrimSpace(containerID))
	now := r.nowTime()
	r.mu.Lock()
	entry, found := r.workloads[key]
	if found && now.Before(entry.expiresAt) {
		r.mu.Unlock()
		return entry.value, entry.err
	}
	if found {
		delete(r.workloads, key)
	}
	if pending := r.workloadCalls[key]; pending != nil {
		r.mu.Unlock()
		select {
		case <-pending.done:
			return pending.value, pending.err
		case <-ctx.Done():
			return KubernetesWorkload{}, ctx.Err()
		}
	}
	pending := &workloadResolveCall{done: make(chan struct{})}
	r.workloadCalls[key] = pending
	r.mu.Unlock()

	var value KubernetesWorkload
	var err error
	if resolver, ok := r.Base.(KubernetesWorkloadResolver); ok {
		value, err = resolver.KubernetesWorkloadForContainer(ctx, containerID)
	} else {
		value.Namespace, err = r.Base.NamespaceForContainer(ctx, containerID)
	}
	completedAt := r.nowTime()
	r.mu.Lock()
	if ctx.Err() == nil && len(r.workloads) >= maxResolverCacheEntries {
		for candidate, candidateEntry := range r.workloads {
			if !completedAt.Before(candidateEntry.expiresAt) {
				delete(r.workloads, candidate)
			}
		}
		for len(r.workloads) >= maxResolverCacheEntries {
			for candidate := range r.workloads {
				delete(r.workloads, candidate)
				break
			}
		}
	}
	if ctx.Err() == nil {
		ttl := r.successTTL(defaultDockerNameTTL)
		if err != nil {
			ttl = r.successTTL(defaultResolverTTL)
		} else if value.Namespace != "" && value.PodName != "" && value.ContainerName != "" {
			ttl = r.successTTL(defaultKubernetesTTL)
		}
		r.workloads[key] = workloadCacheEntry{value: value, err: err, expiresAt: completedAt.Add(ttl)}
	}
	pending.value = value
	pending.err = err
	delete(r.workloadCalls, key)
	close(pending.done)
	r.mu.Unlock()
	return value, err
}

func (r *CachedResolver) resolve(ctx context.Context, cache map[string]cacheEntry, calls map[string]*resolveCall, key string, successTTL time.Duration, call func() (string, error)) (string, error) {
	if r == nil || r.Base == nil {
		return "", errors.New("runtime resolver is unavailable")
	}
	now := r.nowTime()
	r.mu.Lock()
	entry, found := cache[key]
	if found && now.Before(entry.expiresAt) {
		r.mu.Unlock()
		return entry.value, entry.err
	}
	if found {
		delete(cache, key)
	}
	if pending := calls[key]; pending != nil {
		r.mu.Unlock()
		select {
		case <-pending.done:
			return pending.value, pending.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	pending := &resolveCall{done: make(chan struct{})}
	calls[key] = pending
	r.mu.Unlock()

	value, err := call()
	completedAt := r.nowTime()
	r.mu.Lock()
	if ctx.Err() == nil && len(cache) >= maxResolverCacheEntries {
		for candidate, candidateEntry := range cache {
			if !completedAt.Before(candidateEntry.expiresAt) {
				delete(cache, candidate)
			}
		}
		for len(cache) >= maxResolverCacheEntries {
			for candidate := range cache {
				delete(cache, candidate)
				break
			}
		}
	}
	if ctx.Err() == nil {
		ttl := successTTL
		if err != nil {
			ttl = r.successTTL(defaultResolverTTL)
		}
		cache[key] = cacheEntry{value: value, err: err, expiresAt: completedAt.Add(ttl)}
	}
	pending.value = value
	pending.err = err
	delete(calls, key)
	close(pending.done)
	r.mu.Unlock()
	return value, err
}

// successTTL returns r.TTL when it is set, overriding every cache lifetime, and
// otherwise the given default.
func (r *CachedResolver) successTTL(def time.Duration) time.Duration {
	if r.TTL > 0 {
		return r.TTL
	}
	return def
}

func (r *CachedResolver) nowTime() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r CLIResolver) ResolveDockerContainer(ctx context.Context, nameOrID string) (string, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return "", errors.New("container name/id is required")
	}
	ctx, cancel := r.withTimeout(ctx)
	defer cancel()
	out, err := limitedCommandOutput(ctx, "docker", "inspect", "--format", "{{.Id}}", nameOrID)
	if err != nil {
		if commandReportsNotFound(err) {
			return "", fmt.Errorf("%w: docker container", ErrNotFound)
		}
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("docker inspect returned empty container id")
	}
	return strings.ToLower(id), nil
}

func (r CLIResolver) DockerContainerName(ctx context.Context, containerID string) (string, error) {
	containerID = strings.TrimSpace(containerID)
	if containerID == "" {
		return "", errors.New("container id is empty")
	}
	ctx, cancel := r.withTimeout(ctx)
	defer cancel()
	out, err := limitedCommandOutput(ctx, "docker", "inspect", "--format", "{{.Name}}", containerID)
	if err != nil {
		if commandReportsNotFound(err) {
			return "", fmt.Errorf("%w: docker container", ErrNotFound)
		}
		return "", err
	}
	name := strings.TrimPrefix(strings.TrimSpace(string(out)), "/")
	if name == "" {
		return "", errors.New("docker inspect returned empty container name")
	}
	return name, nil
}

func (r CLIResolver) DockerContainers(ctx context.Context) ([]DockerContainer, error) {
	ctx, cancel := r.withTimeout(ctx)
	defer cancel()
	out, err := limitedCommandOutput(ctx, "docker", "ps", "--all", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}")
	if err != nil {
		return nil, err
	}
	return parseDockerContainers(out)
}

// parseDockerContainers reads `docker ps --format '{{.ID}}\t{{.Names}}'`
// output. Names is comma-separated when legacy links add aliases.
func parseDockerContainers(out []byte) ([]DockerContainer, error) {
	var containers []DockerContainer
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, names, _ := strings.Cut(line, "\t")
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			return nil, fmt.Errorf("docker ps returned a container without an id: %q", line)
		}
		container := DockerContainer{ID: id}
		for _, name := range strings.Split(names, ",") {
			if name = strings.TrimPrefix(strings.TrimSpace(name), "/"); name != "" {
				container.Names = append(container.Names, name)
			}
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func (r CLIResolver) NamespaceForContainer(ctx context.Context, containerID string) (string, error) {
	workload, err := r.KubernetesWorkloadForContainer(ctx, containerID)
	return workload.Namespace, err
}

func (r CLIResolver) KubernetesWorkloadForContainer(ctx context.Context, containerID string) (KubernetesWorkload, error) {
	containerID = strings.TrimSpace(containerID)
	if containerID == "" {
		return KubernetesWorkload{}, errors.New("container id is empty")
	}
	workload, criErr := r.workloadFromCRICTL(ctx, containerID)
	if criErr == nil && workload.Namespace != "" {
		if workload.PodName != "" && workload.ContainerName != "" {
			return workload, nil
		}
		fallback, kubectlErr := r.workloadFromKubectl(ctx, containerID)
		if kubectlErr == nil {
			if workload.PodName == "" {
				workload.PodName = fallback.PodName
			}
			if workload.ContainerName == "" {
				workload.ContainerName = fallback.ContainerName
			}
		}
		return workload, nil
	}
	workload, kubectlErr := r.workloadFromKubectl(ctx, containerID)
	if kubectlErr == nil && workload.Namespace != "" {
		return workload, nil
	}
	return KubernetesWorkload{}, combineNamespaceErrors(criErr, kubectlErr)
}

func combineNamespaceErrors(criErr, kubectlErr error) error {
	criAbsent := errors.Is(criErr, ErrNotFound)
	kubectlAbsent := errors.Is(kubectlErr, ErrNotFound)
	criUnavailable := errors.Is(criErr, exec.ErrNotFound)
	kubectlUnavailable := errors.Is(kubectlErr, exec.ErrNotFound)
	if (criAbsent || criUnavailable) && (kubectlAbsent || kubectlUnavailable) && (criAbsent || kubectlAbsent) {
		return fmt.Errorf("%w: kubernetes container", ErrNotFound)
	}
	var infrastructureErrors []error
	for _, err := range []error{criErr, kubectlErr} {
		if err != nil && !errors.Is(err, ErrNotFound) {
			infrastructureErrors = append(infrastructureErrors, err)
		}
	}
	if len(infrastructureErrors) > 0 {
		return errors.Join(infrastructureErrors...)
	}
	return errors.New("kubernetes namespace resolution was inconclusive")
}

func (r CLIResolver) workloadFromCRICTL(ctx context.Context, containerID string) (KubernetesWorkload, error) {
	ctx, cancel := r.withTimeout(ctx)
	defer cancel()
	out, err := limitedCommandOutput(ctx, "crictl", "inspect", containerID)
	if err != nil {
		if commandReportsNotFound(err) {
			return KubernetesWorkload{}, fmt.Errorf("%w: CRI container", ErrNotFound)
		}
		return KubernetesWorkload{}, err
	}
	var raw any
	if err := json.Unmarshal(out, &raw); err != nil {
		return KubernetesWorkload{}, err
	}
	workload := findKubernetesWorkload(raw)
	if workload.Namespace == "" {
		return KubernetesWorkload{}, errors.New("namespace not found in crictl inspect")
	}
	return workload, nil
}

// workloadFromKubectl finds containerID in a cluster-wide pod listing. With a
// shared listing (NewCLIResolver), one listing answers every container for up
// to podListTTL; a miss in a listing older than podListMissRefresh fetches a
// new one before the container is reported absent.
func (r CLIResolver) workloadFromKubectl(ctx context.Context, containerID string) (KubernetesWorkload, error) {
	if r.pods == nil {
		pods, err := r.listPods(ctx)
		if err != nil {
			return KubernetesWorkload{}, err
		}
		return findPodWorkload(pods, containerID)
	}
	fetch := r.listPods
	if r.pods.fetch != nil {
		fetch = r.pods.fetch
	}
	pods, fetchedAt, err := r.pods.get(ctx, podListTTL, fetch)
	if err != nil {
		return KubernetesWorkload{}, err
	}
	workload, err := findPodWorkload(pods, containerID)
	if err == nil || r.pods.nowTime().Sub(fetchedAt) < podListMissRefresh {
		return workload, err
	}
	if pods, _, err = r.pods.get(ctx, podListMissRefresh, fetch); err != nil {
		return KubernetesWorkload{}, err
	}
	return findPodWorkload(pods, containerID)
}

func findPodWorkload(pods []kubePod, containerID string) (KubernetesWorkload, error) {
	short := shortID(containerID)
	for _, pod := range pods {
		if name, ok := matchingContainerName(pod.containerStatuses, containerID, short); ok {
			return KubernetesWorkload{Namespace: pod.namespace, PodName: pod.name, ContainerName: name}, nil
		}
		if name, ok := matchingContainerName(pod.initContainerStatuses, containerID, short); ok {
			return KubernetesWorkload{Namespace: pod.namespace, PodName: pod.name, ContainerName: name}, nil
		}
	}
	return KubernetesWorkload{}, fmt.Errorf("%w: kubernetes container", ErrNotFound)
}

// get returns a pod listing fetched less than maxAge ago, joining an in-flight
// fetch or starting one otherwise. The fetch is detached from the caller's
// cancellation and bounded by the resolver timeout; a caller whose context
// ends stops waiting.
func (c *podListCache) get(ctx context.Context, maxAge time.Duration, fetch func(context.Context) ([]kubePod, error)) ([]kubePod, time.Time, error) {
	c.mu.Lock()
	if c.pods != nil && c.nowTime().Sub(c.fetchedAt) < maxAge {
		pods, at := c.pods, c.fetchedAt
		c.mu.Unlock()
		return pods, at, nil
	}
	call := c.call
	if call == nil {
		call = &podListCall{done: make(chan struct{})}
		c.call = call
		fetchCtx := context.WithoutCancel(ctx)
		go func() {
			pods, err := fetch(fetchCtx)
			c.mu.Lock()
			call.pods, call.err, call.at = pods, err, c.nowTime()
			if err == nil {
				c.pods, c.fetchedAt = pods, call.at
			}
			c.call = nil
			c.mu.Unlock()
			close(call.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-call.done:
		return call.pods, call.at, call.err
	case <-ctx.Done():
		return nil, time.Time{}, ctx.Err()
	}
}

func (c *podListCache) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (r CLIResolver) listPods(ctx context.Context) ([]kubePod, error) {
	ctx, cancel := r.withTimeout(ctx)
	defer cancel()
	out, err := limitedCommandOutput(ctx, "kubectl", "get", "pod", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Status struct {
				ContainerStatuses     []KubeContainerStatus `json:"containerStatuses"`
				InitContainerStatuses []KubeContainerStatus `json:"initContainerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	// Non-nil even when empty so an empty cluster listing is still cached.
	pods := make([]kubePod, 0, len(list.Items))
	for _, item := range list.Items {
		pods = append(pods, kubePod{
			namespace:             item.Metadata.Namespace,
			name:                  item.Metadata.Name,
			containerStatuses:     item.Status.ContainerStatuses,
			initContainerStatuses: item.Status.InitContainerStatuses,
		})
	}
	return pods, nil
}

func (r CLIResolver) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

func limitedCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	output := boundedOutput{limit: maxRuntimeOutputBytes}
	errorOutput := boundedOutput{limit: maxRuntimeErrorBytes}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &output
	cmd.Stderr = &errorOutput
	if err := cmd.Run(); err != nil {
		return nil, &commandError{name: name, err: err, stderr: errorOutput.String(), stderrExceeded: errorOutput.exceeded}
	}
	if output.exceeded {
		return nil, fmt.Errorf("%s output exceeds %d bytes", name, maxRuntimeOutputBytes)
	}
	return output.Bytes(), nil
}

func commandReportsNotFound(err error) bool {
	var commandErr *commandError
	if !errors.As(err, &commandErr) || commandErr.stderrExceeded {
		return false
	}
	stderr := strings.ToLower(commandErr.stderr)
	switch commandErr.name {
	case "docker":
		return strings.Contains(stderr, "no such object") || strings.Contains(stderr, "no such container")
	case "crictl":
		return strings.Contains(stderr, "code = notfound")
	default:
		return false
	}
}

func findNamespace(value any) string {
	return findKubernetesWorkload(value).Namespace
}

func findKubernetesWorkload(value any) KubernetesWorkload {
	root, ok := value.(map[string]any)
	if !ok {
		return KubernetesWorkload{}
	}
	status, ok := root["status"].(map[string]any)
	if !ok {
		return KubernetesWorkload{}
	}
	var workload KubernetesWorkload
	for _, field := range []string{"labels", "annotations"} {
		metadata, ok := status[field].(map[string]any)
		if !ok {
			continue
		}
		if workload.Namespace == "" {
			workload.Namespace = trustedMetadataValue(metadata, "io.kubernetes.pod.namespace")
		}
		if workload.PodName == "" {
			workload.PodName = trustedMetadataValue(metadata, "io.kubernetes.pod.name")
		}
		if workload.ContainerName == "" {
			workload.ContainerName = trustedMetadataValue(metadata, "io.kubernetes.container.name")
		}
	}
	if workload.ContainerName == "" {
		if metadata, ok := status["metadata"].(map[string]any); ok {
			workload.ContainerName = trustedMetadataValue(metadata, "name")
		}
	}
	return workload
}

func trustedMetadataValue(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

type KubeContainerStatus struct {
	Name        string `json:"name"`
	ContainerID string `json:"containerID"`
}

func matchingContainerName(statuses []KubeContainerStatus, full, short string) (string, bool) {
	for _, status := range statuses {
		id := extractID(status.ContainerID)
		if id == "" {
			continue
		}
		if id == full || id == short ||
			(len(id) >= 12 && strings.HasPrefix(full, id)) ||
			(len(short) >= 12 && strings.HasPrefix(id, short)) {
			return strings.TrimSpace(status.Name), true
		}
	}
	return "", false
}

func extractID(value string) string {
	if i := strings.LastIndex(value, "://"); i >= 0 {
		value = value[i+3:]
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
