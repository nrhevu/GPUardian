package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
)

const (
	maxNodeLayoutFileBytes = 512 << 10
	maxNodeGroups          = 64
	maxNodeGroupIDBytes    = 128
	maxNodeGroupNameBytes  = 128
)

type NodeGroupRecord struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	NodeIDs []string `json:"node_ids"`
}

type NodeLayout struct {
	Root   []string          `json:"root"`
	Groups []NodeGroupRecord `json:"groups"`
}

type NodeLayoutStore struct {
	mu     sync.Mutex
	path   string
	loaded bool
	layout NodeLayout
}

func NewNodeLayoutStore(path string) *NodeLayoutStore {
	return &NodeLayoutStore{path: path}
}

func (s *NodeLayoutStore) Get(serverIDs []string) (NodeLayout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	layout, err := s.loadLocked()
	if err != nil {
		return NodeLayout{}, err
	}
	normalized, err := normalizeNodeLayout(layout, serverIDs, false)
	if err != nil {
		return NodeLayout{}, err
	}
	if !reflect.DeepEqual(layout, normalized) {
		if err := s.saveLocked(normalized); err != nil {
			return NodeLayout{}, err
		}
	}
	return cloneNodeLayout(normalized), nil
}

func (s *NodeLayoutStore) Update(layout NodeLayout, serverIDs []string) (NodeLayout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadLocked(); err != nil {
		return NodeLayout{}, err
	}
	normalized, err := normalizeNodeLayout(layout, serverIDs, true)
	if err != nil {
		return NodeLayout{}, err
	}
	if err := s.saveLocked(normalized); err != nil {
		return NodeLayout{}, err
	}
	return cloneNodeLayout(normalized), nil
}

func (s *NodeLayoutStore) loadLocked() (NodeLayout, error) {
	if s.loaded {
		return cloneNodeLayout(s.layout), nil
	}
	data, err := readPrivateFile(s.path, "node layout file", maxNodeLayoutFileBytes)
	if errors.Is(err, os.ErrNotExist) || len(strings.TrimSpace(string(data))) == 0 {
		s.loaded = true
		s.layout = NodeLayout{}
		return NodeLayout{}, nil
	}
	if err != nil {
		return NodeLayout{}, err
	}
	var layout NodeLayout
	if err := json.Unmarshal(data, &layout); err != nil {
		return NodeLayout{}, err
	}
	s.loaded = true
	s.layout = cloneNodeLayout(layout)
	return cloneNodeLayout(layout), nil
}

func (s *NodeLayoutStore) saveLocked(layout NodeLayout) error {
	data, err := json.MarshalIndent(layout, "", "  ")
	if err != nil {
		return err
	}
	committed, err := writePrivateFile(s.path, append(data, '\n'))
	if committed {
		s.loaded = true
		s.layout = cloneNodeLayout(layout)
	}
	return err
}

func normalizeNodeLayout(layout NodeLayout, serverIDs []string, strict bool) (NodeLayout, error) {
	servers := make(map[string]bool, len(serverIDs))
	for _, id := range serverIDs {
		if id != "" {
			servers[id] = true
		}
	}
	if len(layout.Groups) > maxNodeGroups {
		return NodeLayout{}, fmt.Errorf("node group limit reached: maximum %d", maxNodeGroups)
	}

	result := NodeLayout{Root: []string{}, Groups: []NodeGroupRecord{}}
	groups := make(map[string]bool, len(layout.Groups))
	assigned := make(map[string]bool, len(serverIDs))
	for _, input := range layout.Groups {
		group := NodeGroupRecord{ID: strings.TrimSpace(input.ID), Name: strings.TrimSpace(input.Name), NodeIDs: []string{}}
		if group.ID == "" || len(group.ID) > maxNodeGroupIDBytes {
			return NodeLayout{}, fmt.Errorf("node group id must be between 1 and %d bytes", maxNodeGroupIDBytes)
		}
		if !strings.HasPrefix(group.ID, "grp_") {
			return NodeLayout{}, errors.New("node group id must start with grp_")
		}
		if group.Name == "" || len(group.Name) > maxNodeGroupNameBytes {
			return NodeLayout{}, fmt.Errorf("node group name must be between 1 and %d bytes", maxNodeGroupNameBytes)
		}
		if groups[group.ID] {
			return NodeLayout{}, fmt.Errorf("duplicate node group %q", group.ID)
		}
		groups[group.ID] = true
		for _, id := range input.NodeIDs {
			if !servers[id] {
				if strict {
					return NodeLayout{}, fmt.Errorf("unknown node %q", id)
				}
				continue
			}
			if assigned[id] {
				return NodeLayout{}, fmt.Errorf("node %q appears more than once", id)
			}
			assigned[id] = true
			group.NodeIDs = append(group.NodeIDs, id)
		}
		result.Groups = append(result.Groups, group)
	}

	rootSeen := make(map[string]bool, len(layout.Root))
	for _, id := range layout.Root {
		switch {
		case groups[id]:
			if rootSeen[id] {
				return NodeLayout{}, fmt.Errorf("node group %q appears more than once", id)
			}
			rootSeen[id] = true
			result.Root = append(result.Root, id)
		case servers[id]:
			if assigned[id] || rootSeen[id] {
				return NodeLayout{}, fmt.Errorf("node %q appears more than once", id)
			}
			rootSeen[id] = true
			assigned[id] = true
			result.Root = append(result.Root, id)
		default:
			if strict {
				return NodeLayout{}, fmt.Errorf("unknown node layout item %q", id)
			}
		}
	}
	for _, group := range result.Groups {
		if !rootSeen[group.ID] {
			result.Root = append(result.Root, group.ID)
			rootSeen[group.ID] = true
		}
	}
	for _, id := range serverIDs {
		if id != "" && !assigned[id] {
			result.Root = append(result.Root, id)
			assigned[id] = true
		}
	}
	return result, nil
}

func cloneNodeLayout(layout NodeLayout) NodeLayout {
	result := NodeLayout{Root: append([]string(nil), layout.Root...), Groups: make([]NodeGroupRecord, 0, len(layout.Groups))}
	for _, group := range layout.Groups {
		group.NodeIDs = append([]string(nil), group.NodeIDs...)
		result.Groups = append(result.Groups, group)
	}
	return result
}
