// Package core — modelpacks.go: Model Studio filesystem helpers (L109 P811–P813).
//
// G geometry lives under assets/usd/<type>/<MODEL>.usdc (immutable by default).
// S-layer bindings + last lint reports live under artifacts/model-studio/
// so operators can edit semantics without re-exporting USD (L109③).
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ModelPackRoot is the G-layer tree (USD files). Overridable for tests.
var ModelPackRoot = envOrDefault("CIOS_MODEL_PACK_ROOT", "assets/usd")

// ModelStudioDir holds S-layer bindings + lint cache.
var ModelStudioDir = envOrDefault("CIOS_MODEL_STUDIO_DIR", "artifacts/model-studio")

// UsdlintPython is the interpreter used to run tools/usdlint/usdlint.py.
var UsdlintPython = envOrDefault("CIOS_USDLINT_PYTHON", "python3")

// UsdlintScript relative path to the lint tool.
var UsdlintScript = envOrDefault("CIOS_USDLINT_SCRIPT", "tools/usdlint/usdlint.py")

func envOrDefault(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// ModelPackSummary is one row in GET /v1/model-packs.
type ModelPackSummary struct {
	Type         string     `json:"type"`
	Model        string     `json:"model"`
	Path         string     `json:"path"`
	SizeBytes    int64      `json:"size_bytes"`
	ModTime      time.Time  `json:"mod_time"`
	Status       string     `json:"status"`                // ready | pending_conform | lint_unavailable | unknown
	LintResult   string     `json:"lint_result,omitempty"` // pass | fail | unavailable
	LintFail     int        `json:"lint_fail,omitempty"`
	LintPass     int        `json:"lint_pass,omitempty"`
	LintAt       *time.Time `json:"lint_at,omitempty"`
	HasBindings  bool       `json:"has_bindings"`
	BindingCount int        `json:"binding_count"`
}

// ModelBinding is one S-layer prim → semantics mapping.
type ModelBinding struct {
	Prim        string `json:"prim"`
	CiosType    string `json:"cios_type,omitempty"`
	CiosModel   string `json:"cios_model,omitempty"`
	CiosRelpath string `json:"cios_relpath,omitempty"`
	Note        string `json:"note,omitempty"`
}

// ModelBindingsDoc is the side-car S-layer document.
type ModelBindingsDoc struct {
	Type      string         `json:"type"`
	Model     string         `json:"model"`
	UpdatedAt time.Time      `json:"updated_at"`
	UpdatedBy string         `json:"updated_by,omitempty"`
	Notes     string         `json:"notes,omitempty"`
	Bindings  []ModelBinding `json:"bindings"`
}

// ModelLintReport is a stored soft-lint result (usdlint --json or env failure).
type ModelLintReport struct {
	Type       string          `json:"type"`
	Model      string          `json:"model"`
	Path       string          `json:"path"`
	RanAt      time.Time       `json:"ran_at"`
	ExitCode   int             `json:"exit_code"`
	Result     string          `json:"result"` // pass | fail | unavailable
	Summary    json.RawMessage `json:"summary,omitempty"`
	Gates      json.RawMessage `json:"gates,omitempty"`
	RawStdout  string          `json:"raw_stdout,omitempty"`
	RawStderr  string          `json:"raw_stderr,omitempty"`
	SoftStatus string          `json:"soft_status"` // ready | pending_conform | lint_unavailable
}

// ListModelPacks walks ModelPackRoot for *.usdc / *.usda.
func ListModelPacks() ([]ModelPackSummary, error) {
	root := ModelPackRoot
	var out []ModelPackSummary
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".usdc" && ext != ".usda" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// expect <type>/<MODEL>.usdc
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 {
			return nil
		}
		typ, base := parts[0], parts[1]
		model := strings.TrimSuffix(base, filepath.Ext(base))
		// Skip vendor backups / intermediate G-conform artifacts (PRMT-223+).
		if strings.Contains(model, ".vendor") ||
			strings.Contains(model, ".conformed") ||
			strings.Contains(model, ".staging") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum := ModelPackSummary{
			Type:      typ,
			Model:     model,
			Path:      filepath.ToSlash(path),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime().UTC(),
			Status:    "unknown",
		}
		if doc, ok, _ := loadBindings(typ, model); ok {
			sum.HasBindings = true
			sum.BindingCount = len(doc.Bindings)
		}
		if rep, ok, _ := loadLintReport(typ, model); ok {
			sum.LintResult = rep.Result
			sum.LintAt = &rep.RanAt
			sum.Status = rep.SoftStatus
			if rep.Summary != nil {
				var s struct {
					Pass int `json:"pass"`
					Fail int `json:"fail"`
				}
				_ = json.Unmarshal(rep.Summary, &s)
				sum.LintPass, sum.LintFail = s.Pass, s.Fail
			}
		}
		out = append(out, sum)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Model < out[j].Model
	})
	if out == nil {
		out = []ModelPackSummary{}
	}
	return out, nil
}

func packFilePath(typ, model string) (string, error) {
	typ = sanitizeSeg(typ)
	model = sanitizeSeg(model)
	if typ == "" || model == "" {
		return "", fmt.Errorf("core: bad pack id")
	}
	base := filepath.Join(ModelPackRoot, typ, model)
	for _, ext := range []string{".usdc", ".usda"} {
		p := base + ext
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("core: model pack not found: %s/%s", typ, model)
}

// MaxUSDImportBytes is the hard cap for a single pack upload (64 MiB).
const MaxUSDImportBytes = 64 << 20

// ErrUSDImportValidation is returned when usdlint rejects an import.
var ErrUSDImportValidation = errors.New("core: usd import validation failed")

// ErrUSDImportExists is returned when the pack already exists and force=false.
var ErrUSDImportExists = errors.New("core: usd pack already exists")

// ImportUSDPackResult is the wire result of a validated import.
type ImportUSDPackResult struct {
	Type      string          `json:"type"`
	Model     string          `json:"model"`
	Path      string          `json:"path"`
	SizeBytes int64           `json:"size_bytes"`
	Lint      ModelLintReport `json:"lint"`
	Forced    bool            `json:"forced,omitempty"`
}

// ImportUSDPack writes a vendor USD pack under ModelPackRoot after
// usdlint validation. Bytes land on a staging path first; lint runs
// against that path (PRMT-222). Only a passing lint promotes into
// the final pack location — so concurrent export/scene never sees
// unvalidated bytes. force overwrites an existing pack of the same
// type/model after validation succeeds.
func ImportUSDPack(typ, model, filename string, data []byte, force bool) (ImportUSDPackResult, error) {
	typ = sanitizeSeg(typ)
	model = sanitizeSeg(model)
	if typ == "" || model == "" {
		return ImportUSDPackResult{}, fmt.Errorf("core: bad pack id")
	}
	if len(data) == 0 {
		return ImportUSDPackResult{}, fmt.Errorf("core: empty usd payload")
	}
	if len(data) > MaxUSDImportBytes {
		return ImportUSDPackResult{}, fmt.Errorf("core: usd payload exceeds %d bytes", MaxUSDImportBytes)
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".usda"
	}
	if ext != ".usdc" && ext != ".usda" {
		return ImportUSDPackResult{}, fmt.Errorf("core: usd import requires .usda or .usdc, got %q", ext)
	}
	dest := filepath.Join(ModelPackRoot, typ, model+ext)
	if !force {
		if _, err := os.Stat(dest); err == nil {
			return ImportUSDPackResult{}, ErrUSDImportExists
		}
		// also conflict if other extension present
		if p, err := packFilePath(typ, model); err == nil && p != dest {
			return ImportUSDPackResult{}, ErrUSDImportExists
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return ImportUSDPackResult{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "usd-import-*.tmp"+ext)
	if err != nil {
		return ImportUSDPackResult{}, err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return ImportUSDPackResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return ImportUSDPackResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return ImportUSDPackResult{}, err
	}
	// Staging path only — final dest is untouched until lint passes (PRMT-222).
	// Keep the real extension (.usda/.usdc) so usdlint accepts the file;
	// ".usdc.staging" was rejected as extension ".staging".
	stagePath := strings.TrimSuffix(dest, ext) + ".staging" + ext
	_ = os.Remove(stagePath)
	if err := os.Rename(tmpPath, stagePath); err != nil {
		if err := copyFile(tmpPath, stagePath); err != nil {
			return ImportUSDPackResult{}, err
		}
		_ = os.Remove(tmpPath)
	}
	cleanupStage := func() { _ = os.Remove(stagePath) }

	// Lint against staging file path; do not promote yet.
	rep, err := runModelPackLintAt(typ, model, stagePath)
	if err != nil {
		cleanupStage()
		return ImportUSDPackResult{}, err
	}
	// Gate: only pass is accepted. Fail and lint-unavailable both block import
	// so vendor files cannot land without a successful usdlint run.
	if rep.Result == "fail" || rep.ExitCode == 1 {
		cleanupStage()
		return ImportUSDPackResult{Type: typ, Model: model, Lint: rep}, fmt.Errorf("%w: %s", ErrUSDImportValidation, rep.Result)
	}
	if rep.Result == "unavailable" || rep.SoftStatus == "lint_unavailable" {
		cleanupStage()
		return ImportUSDPackResult{Type: typ, Model: model, Lint: rep}, fmt.Errorf("%w: lint unavailable", ErrUSDImportValidation)
	}

	// Promote staging → final only after lint pass.
	if force {
		if prev, err := packFilePath(typ, model); err == nil {
			_ = os.Remove(prev)
		}
	}
	_ = os.Remove(dest)
	if err := os.Rename(stagePath, dest); err != nil {
		if err := copyFile(stagePath, dest); err != nil {
			cleanupStage()
			return ImportUSDPackResult{}, err
		}
		cleanupStage()
	}
	st, _ := os.Stat(dest)
	size := int64(0)
	if st != nil {
		size = st.Size()
	}
	return ImportUSDPackResult{
		Type: typ, Model: model, Path: filepath.ToSlash(dest),
		SizeBytes: size, Lint: rep, Forced: force,
	}, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func sanitizeSeg(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "..") || strings.ContainsAny(s, `/\`) {
		return ""
	}
	return s
}

func bindingsPath(typ, model string) string {
	return filepath.Join(ModelStudioDir, "bindings", typ, model+".json")
}

func lintPath(typ, model string) string {
	return filepath.Join(ModelStudioDir, "lint", typ+"__"+model+".json")
}

func loadBindings(typ, model string) (ModelBindingsDoc, bool, error) {
	p := bindingsPath(typ, model)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ModelBindingsDoc{}, false, nil
		}
		return ModelBindingsDoc{}, false, err
	}
	var doc ModelBindingsDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return ModelBindingsDoc{}, false, err
	}
	if doc.Bindings == nil {
		doc.Bindings = []ModelBinding{}
	}
	return doc, true, nil
}

// SaveBindings writes the S-layer document (creates dirs).
func SaveBindings(typ, model, principal string, notes string, bindings []ModelBinding) (ModelBindingsDoc, error) {
	typ = sanitizeSeg(typ)
	model = sanitizeSeg(model)
	if typ == "" || model == "" {
		return ModelBindingsDoc{}, fmt.Errorf("core: bad pack id")
	}
	if _, err := packFilePath(typ, model); err != nil {
		return ModelBindingsDoc{}, err
	}
	if bindings == nil {
		bindings = []ModelBinding{}
	}
	// Drop empty prim rows.
	clean := make([]ModelBinding, 0, len(bindings))
	for _, b := range bindings {
		b.Prim = strings.TrimSpace(b.Prim)
		if b.Prim == "" {
			continue
		}
		clean = append(clean, b)
	}
	doc := ModelBindingsDoc{
		Type:      typ,
		Model:     model,
		UpdatedAt: time.Now().UTC(),
		UpdatedBy: principal,
		Notes:     notes,
		Bindings:  clean,
	}
	p := bindingsPath(typ, model)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return ModelBindingsDoc{}, err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ModelBindingsDoc{}, err
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return ModelBindingsDoc{}, err
	}
	return doc, nil
}

func loadLintReport(typ, model string) (ModelLintReport, bool, error) {
	p := lintPath(typ, model)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ModelLintReport{}, false, nil
		}
		return ModelLintReport{}, false, err
	}
	var rep ModelLintReport
	if err := json.Unmarshal(b, &rep); err != nil {
		return ModelLintReport{}, false, err
	}
	return rep, true, nil
}

// RunModelPackLint executes usdlint --json (soft): exit 1 → pending_conform, not hard reject.
// Resolves the on-disk pack via packFilePath (promoted packs only).
func RunModelPackLint(typ, model string) (ModelLintReport, error) {
	typ = sanitizeSeg(typ)
	model = sanitizeSeg(model)
	path, err := packFilePath(typ, model)
	if err != nil {
		return ModelLintReport{}, err
	}
	return runModelPackLintAt(typ, model, path)
}

// runModelPackLintAt runs usdlint against an explicit file path (staging or
// promoted). Public RunModelPackLint stays (typ, model)-keyed; import uses
// this so lint can read staging before promote (PRMT-222).
func runModelPackLintAt(typ, model, packPath string) (ModelLintReport, error) {
	typ = sanitizeSeg(typ)
	model = sanitizeSeg(model)
	if typ == "" || model == "" {
		return ModelLintReport{}, fmt.Errorf("core: bad pack id")
	}
	if packPath == "" {
		return ModelLintReport{}, fmt.Errorf("core: empty pack path")
	}
	if _, err := os.Stat(packPath); err != nil {
		return ModelLintReport{}, fmt.Errorf("core: pack path: %w", err)
	}
	absPack, _ := filepath.Abs(packPath)
	script := UsdlintScript
	if !filepath.IsAbs(script) {
		if wd, e := os.Getwd(); e == nil {
			script = filepath.Join(wd, script)
		}
	}
	rep := ModelLintReport{
		Type:  typ,
		Model: model,
		Path:  filepath.ToSlash(packPath),
		RanAt: time.Now().UTC(),
	}
	if _, err := os.Stat(script); err != nil {
		rep.ExitCode = 2
		rep.Result = "unavailable"
		rep.SoftStatus = "lint_unavailable"
		rep.RawStderr = "usdlint script missing: " + script
		_ = saveLintReport(rep)
		return rep, nil
	}
	// M2: bound usdlint so a hung python cannot block the HTTP request.
	lintCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(lintCtx, UsdlintPython, script, absPack, "--json")
	if wd, e := os.Getwd(); e == nil {
		cmd.Dir = wd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	rep.RawStdout = stdout.String()
	rep.RawStderr = stderr.String()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			rep.ExitCode = ee.ExitCode()
		} else {
			rep.ExitCode = 2
			rep.Result = "unavailable"
			rep.SoftStatus = "lint_unavailable"
			_ = saveLintReport(rep)
			return rep, nil
		}
	} else {
		rep.ExitCode = 0
	}
	// Parse JSON if present.
	var parsed struct {
		Gates   json.RawMessage `json:"gates"`
		Summary json.RawMessage `json:"summary"`
		Result  string          `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err == nil {
		rep.Gates = parsed.Gates
		rep.Summary = parsed.Summary
		if parsed.Result != "" {
			rep.Result = parsed.Result
		}
	}
	if rep.Result == "" {
		if rep.ExitCode == 0 {
			rep.Result = "pass"
		} else if rep.ExitCode == 1 {
			rep.Result = "fail"
		} else {
			rep.Result = "unavailable"
		}
	}
	switch rep.Result {
	case "pass":
		rep.SoftStatus = "ready"
	case "fail":
		rep.SoftStatus = "pending_conform"
	default:
		rep.SoftStatus = "lint_unavailable"
	}
	if err := saveLintReport(rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func saveLintReport(rep ModelLintReport) error {
	p := lintPath(rep.Type, rep.Model)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o644)
}

// GetModelPackDetail returns pack summary + optional bindings + lint.
func GetModelPackDetail(typ, model string) (ModelPackSummary, ModelBindingsDoc, *ModelLintReport, error) {
	path, err := packFilePath(typ, model)
	if err != nil {
		return ModelPackSummary{}, ModelBindingsDoc{}, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return ModelPackSummary{}, ModelBindingsDoc{}, nil, err
	}
	sum := ModelPackSummary{
		Type:      typ,
		Model:     model,
		Path:      filepath.ToSlash(path),
		SizeBytes: info.Size(),
		ModTime:   info.ModTime().UTC(),
		Status:    "unknown",
	}
	doc, hasB, err := loadBindings(typ, model)
	if err != nil {
		return ModelPackSummary{}, ModelBindingsDoc{}, nil, err
	}
	if hasB {
		sum.HasBindings = true
		sum.BindingCount = len(doc.Bindings)
	} else {
		doc = ModelBindingsDoc{Type: typ, Model: model, Bindings: []ModelBinding{}}
	}
	rep, hasL, err := loadLintReport(typ, model)
	if err != nil {
		return ModelPackSummary{}, ModelBindingsDoc{}, nil, err
	}
	var repPtr *ModelLintReport
	if hasL {
		repPtr = &rep
		sum.LintResult = rep.Result
		sum.LintAt = &rep.RanAt
		sum.Status = rep.SoftStatus
	}
	return sum, doc, repPtr, nil
}
