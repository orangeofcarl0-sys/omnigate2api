// Profile 注册表与外部加载（SPEC §4.4）：
// 加载顺序 = 内置注册表 → 外部 YAML(覆盖同名/新增)；校验失败即拒绝(fail-fast)。
package adapt

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Registry Profile 注册表。
type Registry struct {
	mu       sync.RWMutex
	profiles map[string]*UpstreamProfile
	order    []string // 注册顺序（供路由展示）
}

// NewRegistry 以内置表初始化。
func NewRegistry(builtins ...*UpstreamProfile) *Registry {
	r := &Registry{profiles: map[string]*UpstreamProfile{}}
	for _, p := range builtins {
		_ = r.Register(p)
	}
	return r
}

// Register 注册（校验失败即拒绝，不覆盖既有项）。
func (r *Registry) Register(p *UpstreamProfile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.profiles[p.ID]; !ok {
		r.order = append(r.order, p.ID)
	}
	r.profiles[p.ID] = p
	return nil
}

// Get 取 Profile；未注册返回 nil。
func (r *Registry) Get(id string) *UpstreamProfile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.profiles[id]
}

// IDs 已注册 ID 列表（注册顺序）。
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	sort.Strings(out)
	return out
}

// LoadDir 加载目录下全部 *.yaml 并注册（外部覆盖同名内置项）。
// 任一文件非法即返回错误（fail-fast，不加载部分状态）。
func (r *Registry) LoadDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("profiles dir %q: %w", dir, err)
	}
	var pending []*UpstreamProfile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p, err := loadProfileFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		pending = append(pending, p)
	}
	for _, p := range pending {
		if err := r.Register(p); err != nil {
			return err
		}
	}
	return nil
}

func loadProfileFile(path string) (*UpstreamProfile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load profile %q: %w", path, err)
	}
	var p UpstreamProfile
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse profile %q: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("profile %q: %w", path, err)
	}
	return &p, nil
}
