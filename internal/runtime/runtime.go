package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"
)

type Resolver interface {
	ResolveDockerContainer(ctx context.Context, nameOrID string) (string, error)
	DockerContainerName(ctx context.Context, containerID string) (string, error)
	NamespaceForContainer(ctx context.Context, containerID string) (string, error)
}

type CLIResolver struct {
	Timeout time.Duration
}

func (r CLIResolver) ResolveDockerContainer(ctx context.Context, nameOrID string) (string, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return "", errors.New("container name/id is required")
	}
	ctx, cancel := r.withTimeout(ctx)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Id}}", nameOrID).Output()
	if err != nil {
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
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Name}}", containerID).Output()
	if err != nil {
		return "", err
	}
	name := strings.TrimPrefix(strings.TrimSpace(string(out)), "/")
	if name == "" {
		return "", errors.New("docker inspect returned empty container name")
	}
	return name, nil
}

func (r CLIResolver) NamespaceForContainer(ctx context.Context, containerID string) (string, error) {
	containerID = strings.TrimSpace(containerID)
	if containerID == "" {
		return "", errors.New("container id is empty")
	}
	if ns, err := r.namespaceFromCRICTL(ctx, containerID); err == nil && ns != "" {
		return ns, nil
	}
	return r.namespaceFromKubectl(ctx, containerID)
}

func (r CLIResolver) namespaceFromCRICTL(ctx context.Context, containerID string) (string, error) {
	ctx, cancel := r.withTimeout(ctx)
	defer cancel()
	out, err := exec.CommandContext(ctx, "crictl", "inspect", containerID).Output()
	if err != nil {
		return "", err
	}
	var raw any
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", err
	}
	ns := findNamespace(raw)
	if ns == "" {
		return "", errors.New("namespace not found in crictl inspect")
	}
	return ns, nil
}

func (r CLIResolver) namespaceFromKubectl(ctx context.Context, containerID string) (string, error) {
	ctx, cancel := r.withTimeout(ctx)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "get", "pod", "-A", "-o", "json").Output()
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Status struct {
				ContainerStatuses     []KubeContainerStatus `json:"containerStatuses"`
				InitContainerStatuses []KubeContainerStatus `json:"initContainerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", err
	}
	short := shortID(containerID)
	for _, item := range list.Items {
		if statusHasContainer(item.Status.ContainerStatuses, containerID, short) ||
			statusHasContainer(item.Status.InitContainerStatuses, containerID, short) {
			return item.Metadata.Namespace, nil
		}
	}
	return "", errors.New("container namespace not found")
}

func (r CLIResolver) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

func findNamespace(value any) string {
	switch v := value.(type) {
	case map[string]any:
		if labels, ok := v["labels"].(map[string]any); ok {
			if ns, ok := labels["io.kubernetes.pod.namespace"].(string); ok && ns != "" {
				return ns
			}
		}
		if annotations, ok := v["annotations"].(map[string]any); ok {
			if ns, ok := annotations["io.kubernetes.pod.namespace"].(string); ok && ns != "" {
				return ns
			}
		}
		if ns, ok := v["namespace"].(string); ok && ns != "" {
			return ns
		}
		for _, child := range v {
			if ns := findNamespace(child); ns != "" {
				return ns
			}
		}
	case []any:
		for _, child := range v {
			if ns := findNamespace(child); ns != "" {
				return ns
			}
		}
	}
	return ""
}

type KubeContainerStatus struct {
	ContainerID string `json:"containerID"`
}

func statusHasContainer(statuses []KubeContainerStatus, full, short string) bool {
	for _, status := range statuses {
		id := extractID(status.ContainerID)
		if id == "" {
			continue
		}
		if id == full || id == short || strings.HasPrefix(full, id) || strings.HasPrefix(id, short) {
			return true
		}
	}
	return false
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
