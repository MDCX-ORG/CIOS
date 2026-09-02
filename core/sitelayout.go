// Package core — sitelayout.go: Site-Draw Web layout documents (L109 P821–P825).
//
// Layout is stored under artifacts/model-studio/layouts/{site}.json (L layer).
// Writeback uses existing PutAsset (CMDB) for instances; edges stay on the
// layout document until a full topology store exists.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// LayoutInstance is one placed equipment unit on the 2D canvas.
type LayoutInstance struct {
	ID       string  `json:"id"`
	Path     string  `json:"path"` // CMDB path e.g. sgp01.pod000
	Type     string  `json:"type"` // types.yaml leaf type
	Model    string  `json:"model,omitempty"`
	PackType string  `json:"pack_type,omitempty"` // assets/usd/<pack_type>/
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Rot      float64 `json:"rot"` // degrees
}

// LayoutEdge is a connection between two instances.
type LayoutEdge struct {
	ID       string `json:"id"`
	FromID   string `json:"from_id"`
	ToID     string `json:"to_id"`
	Relation string `json:"relation"` // protocol types.yaml relations
}

// SiteLayout is the L-layer document for one site.
type SiteLayout struct {
	Site      string           `json:"site"`
	Instances []LayoutInstance `json:"instances"`
	Edges     []LayoutEdge     `json:"edges"`
	UpdatedAt time.Time        `json:"updated_at"`
	UpdatedBy string           `json:"updated_by,omitempty"`
	// Last writeback summary (optional).
	LastWriteback *LayoutWritebackResult `json:"last_writeback,omitempty"`
}

// LayoutWritebackResult is produced by POST …:writeback.
type LayoutWritebackResult struct {
	At            time.Time `json:"at"`
	Actor         string    `json:"actor"`
	AssetsCreated int       `json:"assets_created"`
	AssetsUpdated int       `json:"assets_updated"`
	EdgesKept     int       `json:"edges_kept"`
	Errors        []string  `json:"errors,omitempty"`
}

func layoutPath(site string) string {
	return filepath.Join(ModelStudioDir, "layouts", sanitizeSeg(site)+".json")
}

// LoadSiteLayout returns empty layout if missing.
func LoadSiteLayout(site string) (SiteLayout, error) {
	site = sanitizeSeg(site)
	if site == "" {
		return SiteLayout{}, fmt.Errorf("core: bad site slug")
	}
	p := layoutPath(site)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return SiteLayout{
				Site:      site,
				Instances: []LayoutInstance{},
				Edges:     []LayoutEdge{},
			}, nil
		}
		return SiteLayout{}, err
	}
	var lay SiteLayout
	if err := json.Unmarshal(b, &lay); err != nil {
		return SiteLayout{}, err
	}
	lay.Site = site
	if lay.Instances == nil {
		lay.Instances = []LayoutInstance{}
	}
	if lay.Edges == nil {
		lay.Edges = []LayoutEdge{}
	}
	return lay, nil
}

// normalizeValidateLayout trims fields and enforces identity/path/type rules.
// When dict is non-nil, type ∈ types.yaml and path must parse (H3).
func normalizeValidateLayout(lay *SiteLayout, dict *cpath.Dict) error {
	site := sanitizeSeg(lay.Site)
	if site == "" || !validSiteSlug(site) {
		return fmt.Errorf("core: invalid site slug")
	}
	lay.Site = site
	if lay.Instances == nil {
		lay.Instances = []LayoutInstance{}
	}
	if lay.Edges == nil {
		lay.Edges = []LayoutEdge{}
	}
	vocab := typeVocab(dict)
	relVocab := relationVocab(dict)
	ids := map[string]struct{}{}
	paths := map[string]string{}
	for i := range lay.Instances {
		inst := &lay.Instances[i]
		inst.ID = strings.TrimSpace(inst.ID)
		inst.Path = strings.TrimSpace(inst.Path)
		inst.Type = strings.ToLower(strings.TrimSpace(inst.Type))
		if inst.ID == "" {
			return fmt.Errorf("core: instance missing id")
		}
		if _, dup := ids[inst.ID]; dup {
			return fmt.Errorf("core: duplicate instance id %s", inst.ID)
		}
		ids[inst.ID] = struct{}{}
		if inst.Path == "" || inst.Type == "" {
			return fmt.Errorf("core: instance %s needs path and type", inst.ID)
		}
		if len(vocab) > 0 {
			if _, ok := vocab[inst.Type]; !ok {
				return fmt.Errorf("core: instance %s type %q not in protocol types.yaml", inst.ID, inst.Type)
			}
		}
		if dict != nil {
			if _, err := dict.ParseAssetPath(inst.Path); err != nil {
				return fmt.Errorf("core: instance %s path %q: %w", inst.ID, inst.Path, err)
			}
		}
		if !strings.HasPrefix(inst.Path, site+".") && inst.Path != site {
			if inst.Type != "site" {
				return fmt.Errorf("core: path %s must be under site %s", inst.Path, site)
			}
		}
		if prev, ok := paths[inst.Path]; ok {
			return fmt.Errorf("core: path %s used by %s and %s", inst.Path, prev, inst.ID)
		}
		paths[inst.Path] = inst.ID
	}
	for i := range lay.Edges {
		e := &lay.Edges[i]
		e.ID = strings.TrimSpace(e.ID)
		e.FromID = strings.TrimSpace(e.FromID)
		e.ToID = strings.TrimSpace(e.ToID)
		e.Relation = strings.ToLower(strings.TrimSpace(e.Relation))
		if e.ID == "" || e.FromID == "" || e.ToID == "" {
			return fmt.Errorf("core: edge incomplete")
		}
		if e.FromID == e.ToID {
			return fmt.Errorf("core: edge self-loop")
		}
		if _, ok := ids[e.FromID]; !ok {
			return fmt.Errorf("core: edge from unknown instance %s", e.FromID)
		}
		if _, ok := ids[e.ToID]; !ok {
			return fmt.Errorf("core: edge to unknown instance %s", e.ToID)
		}
		// Relation vocabulary from protocol types.yaml (PRMT-223).
		// When dict is nil / Relations empty, skip (mirrors typeVocab).
		if len(relVocab) > 0 {
			if _, ok := relVocab[e.Relation]; !ok {
				return fmt.Errorf("core: bad relation %q (not in protocol types.yaml relations)", e.Relation)
			}
		}
	}
	return nil
}

// relationVocab returns lowercased relation keys from types.yaml.
// Empty when dict is nil or Relations not loaded.
func relationVocab(dict *cpath.Dict) map[string]struct{} {
	out := map[string]struct{}{}
	if dict == nil {
		return out
	}
	for name := range dict.Relations {
		out[strings.ToLower(name)] = struct{}{}
	}
	return out
}

// LayoutRelationNames returns sorted relation keys from the protocol
// dictionary for admin UI vocab feeds (PRMT-223). Never nil.
func LayoutRelationNames(dict *cpath.Dict) []string {
	v := relationVocab(dict)
	out := make([]string, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SaveSiteLayout validates and persists the layout document.
// When dict is non-nil, instance type must be in protocol types.yaml
// and path must parse as a cpath asset path (L109 H3 / pkg/cpath).
func SaveSiteLayout(lay SiteLayout, actor string, dict *cpath.Dict) (SiteLayout, error) {
	if err := normalizeValidateLayout(&lay, dict); err != nil {
		return SiteLayout{}, err
	}
	lay.UpdatedAt = time.Now().UTC()
	lay.UpdatedBy = actor
	p := layoutPath(lay.Site)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return SiteLayout{}, err
	}
	raw, err := json.MarshalIndent(lay, "", "  ")
	if err != nil {
		return SiteLayout{}, err
	}
	if err := writeJSONAtomic(p, raw); err != nil {
		return SiteLayout{}, err
	}
	return lay, nil
}

// writeJSONAtomic writes via tmp+rename (mirror fileStore.save).
func writeJSONAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-layout-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// cloneSpec shallow-copies a CMDB asset Spec map.
func cloneSpec(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// WritebackSiteLayout creates/updates CMDB assets for each instance and
// stores relation edges on the layout (topology table not yet in core).
//
// H1: when the asset already exists, merge layout/layout_edges (and
// optional model/pack_type) into the existing Spec — never clobber
// lifecycle or other curated fields with a full replace.
func WritebackSiteLayout(ctx context.Context, st Store, lay SiteLayout, actor string, dict *cpath.Dict) (SiteLayout, LayoutWritebackResult, error) {
	res := LayoutWritebackResult{
		At:    time.Now().UTC(),
		Actor: actor,
	}
	if err := normalizeValidateLayout(&lay, dict); err != nil {
		return lay, res, err
	}
	// Note: cpath requires ≥1 node under site (site-only path illegal).
	// Instances must be site.typeNNN (e.g. sgp01.pod000).
	for _, inst := range lay.Instances {
		if inst.Path == lay.Site || !strings.Contains(inst.Path, ".") {
			res.Errors = append(res.Errors, inst.Path+": path must be site.<type><index> (not site-only)")
			continue
		}
		existing, existed, gerr := st.GetAsset(ctx, inst.Path)
		if gerr != nil {
			res.Errors = append(res.Errors, inst.Path+": "+gerr.Error())
			continue
		}
		var spec map[string]any
		if existed && existing.Spec != nil {
			spec = cloneSpec(existing.Spec)
		} else {
			spec = map[string]any{
				"type":      inst.Type,
				"lifecycle": "planned",
				"source":    "site-draw",
			}
		}
		// Merge layout keys only on update; fill type if missing.
		if _, ok := spec["type"]; !ok || fmt.Sprint(spec["type"]) == "" {
			spec["type"] = inst.Type
		}
		spec["layout"] = map[string]any{
			"x":   inst.X,
			"y":   inst.Y,
			"rot": inst.Rot,
		}
		if inst.Model != "" {
			spec["model"] = inst.Model
		}
		if inst.PackType != "" {
			spec["pack_type"] = inst.PackType
		}
		if !existed {
			spec["source"] = "site-draw"
			if _, ok := spec["lifecycle"]; !ok {
				spec["lifecycle"] = "planned"
			}
		}
		// Attach outgoing edges for this instance into spec for discoverability.
		var outs []map[string]string
		for _, e := range lay.Edges {
			if e.FromID != inst.ID {
				continue
			}
			// resolve to path
			toPath := ""
			for _, o := range lay.Instances {
				if o.ID == e.ToID {
					toPath = o.Path
					break
				}
			}
			if toPath != "" {
				outs = append(outs, map[string]string{"to": toPath, "relation": e.Relation})
			}
		}
		if len(outs) > 0 {
			spec["layout_edges"] = outs
		} else {
			delete(spec, "layout_edges")
		}
		// expectVersion 0 = create or unconditional update (store contract)
		_, err := st.PutAsset(ctx, Asset{Path: inst.Path, Spec: spec}, 0)
		if err != nil {
			res.Errors = append(res.Errors, inst.Path+": "+err.Error())
			continue
		}
		if existed {
			res.AssetsUpdated++
		} else {
			res.AssetsCreated++
		}
	}
	res.EdgesKept = len(lay.Edges)
	lay.LastWriteback = &res
	saved, err := SaveSiteLayout(lay, actor, dict)
	if err != nil {
		return lay, res, err
	}
	return saved, res, nil
}

// NextLayoutPath suggests site.typeNNN free path under site.
func NextLayoutPath(lay SiteLayout, typ string) string {
	typ = strings.ToLower(strings.TrimSpace(typ))
	site := lay.Site
	used := map[string]struct{}{}
	for _, inst := range lay.Instances {
		used[inst.Path] = struct{}{}
	}
	// device-level: type + 3 digits
	for i := 0; i < 1000; i++ {
		p := fmt.Sprintf("%s.%s%03d", site, typ, i)
		if _, ok := used[p]; !ok {
			return p
		}
	}
	return fmt.Sprintf("%s.%s%03d", site, typ, len(lay.Instances))
}

var layoutIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// NewLayoutInstanceID returns a short unique id.
func NewLayoutInstanceID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()%1e12)
}

// ListSiteLayoutSites lists site slugs that have a layout file.
func ListSiteLayoutSites() ([]string, error) {
	dir := filepath.Join(ModelStudioDir, "layouts")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".json") {
			out = append(out, strings.TrimSuffix(name, ".json"))
		}
	}
	sort.Strings(out)
	return out, nil
}
