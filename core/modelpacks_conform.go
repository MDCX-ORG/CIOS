// Package core — modelpacks_conform.go: P814 G8 vocabulary conform (L109).
//
// Product lock (Yuri 2026-07-16):
//  1. Never overwrite vendor G-layer .usdc — optional future *.conformed.usdc only
//  2. Auto scope = G8 vocabulary only
//  3. S-first: write corrected cios_type into S-layer bindings
//  4. UI required (HTTP + admin page)
//  5. Engineer: audit JSON under artifacts/model-studio/conform/; no pxr required
package core

import (
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

// Common vendor / Blender labels → types.yaml keys (G8 assist only).
var g8AliasMap = map[string]string{
	// Only map to keys that exist in protocol/types.yaml (or ext.d).
	"picv":       "valve",
	"dpbv":       "valve",
	"dlc":        "rack",
	"tankslot":   "tank",
	"powershelf": "pdu",
	"liq":        "cdu",
	"coolant":    "cdu",
	"crah":       "fan", // top-mounted air unit → nearest device type in vocab
}

// ConformProposal is one G8 fix row (dry-run or applied).
type ConformProposal struct {
	Prim       string `json:"prim"`
	FromType   string `json:"from_type"`
	ToType     string `json:"to_type"`
	Source     string `json:"source"` // lint | binding | alias | stem
	Action     string `json:"action"` // apply | skip
	SkipReason string `json:"skip_reason,omitempty"`
}

// ConformReport is the P814 result + audit record.
type ConformReport struct {
	Type             string            `json:"type"`
	Model            string            `json:"model"`
	Mode             string            `json:"mode"`     // g8
	Strategy         string            `json:"strategy"` // s_first
	DryRun           bool              `json:"dry_run"`
	Actor            string            `json:"actor"`
	At               time.Time         `json:"at"`
	GLayerAction     string            `json:"g_layer_action"` // none | deferred_conformed_usd
	ConformedUSDPath string            `json:"conformed_usd_path,omitempty"`
	SLayerPath       string            `json:"s_layer_path,omitempty"`
	Proposals        []ConformProposal `json:"proposals"`
	AppliedCount     int               `json:"applied_count"`
	SkippedCount     int               `json:"skipped_count"`
	BindingsAfter    int               `json:"bindings_after"`
	PlatformReady    bool              `json:"platform_ready"` // all binding types in vocab
	Note             string            `json:"note,omitempty"`
}

// RunG8Conform builds proposals from lint G8 + existing bindings, optionally applies to S-layer.
func RunG8Conform(dict *cpath.Dict, typ, model, actor string, dryRun bool, overrides map[string]string) (ConformReport, error) {
	typ = sanitizeSeg(typ)
	model = sanitizeSeg(model)
	if typ == "" || model == "" {
		return ConformReport{}, fmt.Errorf("core: bad pack id")
	}
	if _, err := packFilePath(typ, model); err != nil {
		return ConformReport{}, err
	}
	vocab := typeVocab(dict)
	if len(vocab) == 0 {
		return ConformReport{}, fmt.Errorf("core: empty type vocabulary (dict not loaded)")
	}

	// Seed candidates: existing bindings + parse last G8 lint detail.
	doc, _, err := loadBindings(typ, model)
	if err != nil {
		return ConformReport{}, err
	}
	if doc.Bindings == nil {
		doc.Bindings = []ModelBinding{}
	}
	byPrim := map[string]ModelBinding{}
	for _, b := range doc.Bindings {
		if b.Prim != "" {
			byPrim[b.Prim] = b
		}
	}

	// Lint G8 detail (may be only first failure — still useful).
	if rep, ok, _ := loadLintReport(typ, model); ok && len(rep.Gates) > 0 {
		var gates []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(rep.Gates, &gates) == nil {
			for _, g := range gates {
				if g.ID == "G8" && g.Status == "FAIL" {
					prim, from := parseG8Detail(g.Detail)
					if prim != "" {
						if _, exists := byPrim[prim]; !exists {
							byPrim[prim] = ModelBinding{Prim: prim, CiosType: from}
						} else if byPrim[prim].CiosType == "" && from != "" {
							b := byPrim[prim]
							b.CiosType = from
							byPrim[prim] = b
						}
					}
				}
			}
		}
	}

	// Also consider binding rows whose type is out of vocab.
	var proposals []ConformProposal
	for _, b := range byPrim {
		from := strings.ToLower(strings.TrimSpace(b.CiosType))
		// Prefer override
		if ov, ok := overrides[b.Prim]; ok && strings.TrimSpace(ov) != "" {
			to := strings.ToLower(strings.TrimSpace(ov))
			prop := ConformProposal{Prim: b.Prim, FromType: from, ToType: to, Source: "override"}
			if _, ok := vocab[to]; !ok {
				prop.Action = "skip"
				prop.SkipReason = "override not in types.yaml vocabulary"
			} else {
				prop.Action = "apply"
			}
			proposals = append(proposals, prop)
			continue
		}
		if from == "" {
			// Try stem from prim leaf name
			leaf := primLeaf(b.Prim)
			stem, _ := splitTypeIndex(leaf)
			to, src := suggestType(stem, vocab)
			prop := ConformProposal{Prim: b.Prim, FromType: "", ToType: to, Source: src}
			if to == "" {
				prop.Action = "skip"
				prop.SkipReason = "no vocabulary suggestion for empty type"
			} else {
				prop.Action = "apply"
			}
			proposals = append(proposals, prop)
			continue
		}
		if _, ok := vocab[from]; ok {
			// Already valid — no-op
			continue
		}
		to, src := suggestType(from, vocab)
		prop := ConformProposal{Prim: b.Prim, FromType: from, ToType: to, Source: src}
		if to == "" {
			prop.Action = "skip"
			prop.SkipReason = "no vocabulary suggestion"
		} else {
			prop.Action = "apply"
		}
		proposals = append(proposals, prop)
	}
	sort.Slice(proposals, func(i, j int) bool { return proposals[i].Prim < proposals[j].Prim })

	rep := ConformReport{
		Type:         typ,
		Model:        model,
		Mode:         "g8",
		Strategy:     "s_first",
		DryRun:       dryRun,
		Actor:        actor,
		At:           time.Now().UTC(),
		GLayerAction: "none", // never overwrite vendor USD; pxr rewrite deferred
		Proposals:    proposals,
		Note:         "G8 only; S-layer bindings updated. Raw USD lint may still FAIL until G-layer rewrite (optional *.conformed.usdc later).",
	}
	for _, p := range proposals {
		if p.Action == "apply" {
			rep.AppliedCount++
		} else {
			rep.SkippedCount++
		}
	}

	if dryRun {
		rep.BindingsAfter = len(doc.Bindings)
		rep.PlatformReady = bindingsPlatformReady(doc.Bindings, vocab)
		_ = saveConformAudit(rep)
		return rep, nil
	}

	// Apply to S-layer
	for _, p := range proposals {
		if p.Action != "apply" || p.ToType == "" {
			continue
		}
		b := byPrim[p.Prim]
		b.Prim = p.Prim
		b.CiosType = p.ToType
		if b.CiosModel == "" {
			b.CiosModel = model
		}
		byPrim[p.Prim] = b
	}
	merged := make([]ModelBinding, 0, len(byPrim))
	for _, b := range byPrim {
		merged = append(merged, b)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Prim < merged[j].Prim })
	notes := doc.Notes
	if notes != "" {
		notes += "\n"
	}
	notes += fmt.Sprintf("P814 G8 conform by %s at %s", actor, rep.At.Format(time.RFC3339))
	saved, err := SaveBindings(typ, model, actor, notes, merged)
	if err != nil {
		return rep, err
	}
	rep.SLayerPath = filepath.ToSlash(bindingsPath(typ, model))
	rep.BindingsAfter = len(saved.Bindings)
	rep.PlatformReady = bindingsPlatformReady(saved.Bindings, vocab)
	if err := saveConformAudit(rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func typeVocab(dict *cpath.Dict) map[string]struct{} {
	out := map[string]struct{}{}
	if dict == nil {
		return out
	}
	for name := range dict.Types {
		out[strings.ToLower(name)] = struct{}{}
	}
	return out
}

func bindingsPlatformReady(bs []ModelBinding, vocab map[string]struct{}) bool {
	if len(bs) == 0 {
		return false
	}
	for _, b := range bs {
		t := strings.ToLower(strings.TrimSpace(b.CiosType))
		if t == "" {
			return false
		}
		if _, ok := vocab[t]; !ok {
			return false
		}
	}
	return true
}

func suggestType(raw string, vocab map[string]struct{}) (string, string) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	if s == "" {
		return "", ""
	}
	if _, ok := vocab[s]; ok {
		return s, "exact"
	}
	if a, ok := g8AliasMap[s]; ok {
		if _, ok := vocab[a]; ok {
			return a, "alias"
		}
	}
	// Strip trailing digits: pump000 → pump
	stem, _ := splitTypeIndex(s)
	if stem != s {
		if _, ok := vocab[stem]; ok {
			return stem, "stem"
		}
		if a, ok := g8AliasMap[stem]; ok {
			if _, ok := vocab[a]; ok {
				return a, "alias"
			}
		}
	}
	// Prefix match shortest vocab key
	var best string
	for k := range vocab {
		if strings.HasPrefix(s, k) || strings.HasPrefix(k, s) {
			if best == "" || len(k) < len(best) {
				best = k
			}
		}
	}
	if best != "" {
		return best, "prefix"
	}
	return "", ""
}

var trailingDigits = regexp.MustCompile(`^(.*?)(\d+)$`)

func splitTypeIndex(name string) (stem, idx string) {
	name = strings.ToLower(name)
	m := trailingDigits.FindStringSubmatch(name)
	if m == nil {
		return name, ""
	}
	return m[1], m[2]
}

func primLeaf(prim string) string {
	prim = strings.Trim(prim, "/")
	if i := strings.LastIndex(prim, "/"); i >= 0 {
		return prim[i+1:]
	}
	return prim
}

// parseG8Detail extracts path and type from usdlint G8 detail strings.
// Example: `/root/geo/foo: type 'CRAH' not in vocabulary closed set`
func parseG8Detail(detail string) (prim, fromType string) {
	detail = strings.TrimSpace(detail)
	// type 'X'
	if i := strings.Index(detail, "type '"); i >= 0 {
		rest := detail[i+len("type '"):]
		if j := strings.Index(rest, "'"); j >= 0 {
			fromType = rest[:j]
		}
	}
	// path before :
	if i := strings.Index(detail, ":"); i > 0 {
		prim = strings.TrimSpace(detail[:i])
	}
	// normalize prim: drop leading /root/ optional keep full path for bindings
	return prim, strings.ToLower(fromType)
}

func saveConformAudit(rep ConformReport) error {
	p := filepath.Join(ModelStudioDir, "conform", rep.Type+"__"+rep.Model+"__"+rep.At.Format("20060102T150405")+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// Also write latest pointer
	latest := filepath.Join(ModelStudioDir, "conform", rep.Type+"__"+rep.Model+"__latest.json")
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return err
	}
	return os.WriteFile(latest, raw, 0o644)
}

// LoadTypeVocabList returns sorted type names for UI dropdowns.
func LoadTypeVocabList(dict *cpath.Dict) []string {
	if dict == nil {
		return nil
	}
	out := make([]string, 0, len(dict.Types))
	for k := range dict.Types {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
