// Package core — USD model pack import/export (vendor handoff).
package core

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelPackExport_RoundTripBytes(t *testing.T) {
	// Commercial USD packs (assets/usd/**) are not published in the open-core
	// tree. Exercise export against a temp fixture so the gate still covers
	// the round-trip without depending on proprietary binaries.
	root := filepath.Join(t.TempDir(), "usd")
	if err := os.MkdirAll(filepath.Join(root, "pod"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("cios-oss-modelpack-export-fixture")
	if err := os.WriteFile(filepath.Join(root, "pod", "AC45.usdc"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	prev := ModelPackRoot
	ModelPackRoot = root
	t.Cleanup(func() { ModelPackRoot = prev })

	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	r := doOrgReq(t, ts, http.MethodGet, "/v1/model-packs/pod/AC45:export", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("export: %d %s", r.code, r.body)
	}
	if r.body != string(want) {
		if len(r.body) != len(want) {
			t.Fatalf("export len=%d want %d", len(r.body), len(want))
		}
		t.Fatalf("export body mismatch")
	}
}

func TestModelPackImport_ValidationGate(t *testing.T) {
	dir := t.TempDir()
	prevRoot := ModelPackRoot
	prevStudio := ModelStudioDir
	ModelPackRoot = filepath.Join(dir, "usd")
	ModelStudioDir = filepath.Join(dir, "studio")
	t.Cleanup(func() {
		ModelPackRoot = prevRoot
		ModelStudioDir = prevStudio
	})
	// Point usdlint at real script if present; if missing, import must 422 unavailable.
	prevScript := UsdlintScript
	UsdlintScript = filepath.Join(moduleRoot(t), "tools", "usdlint", "usdlint.py")
	t.Cleanup(func() { UsdlintScript = prevScript })

	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Minimal USDA that may or may not pass usdlint; we still exercise the path.
	// Empty garbage should fail validation or lint unavailable.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("type", "cdu")
	_ = w.WriteField("model", "TESTIMPORT01")
	part, err := w.CreateFormFile("file", "TESTIMPORT01.usda")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("#usda 1.0\n(\n    defaultPrim = \"Root\"\n)\n\ndef Xform \"Root\"\n{\n}\n"))
	_ = w.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/model-packs", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+adminTok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	// Accept 201 (pass), 422 (lint fail/unavailable), or 400 — must not 500.
	if res.StatusCode == http.StatusInternalServerError {
		t.Fatalf("import 500: %s", body)
	}
	t.Logf("import status=%d body=%s", res.StatusCode, truncateStr(string(body), 200))

	// Export missing pack → 404
	r := doOrgReq(t, ts, http.MethodGet, "/v1/model-packs/cdu/NO_SUCH:export", "", adminTok, "")
	if r.code != http.StatusNotFound {
		t.Fatalf("export missing: %d %s", r.code, r.body)
	}
}

// TestImportUSDPack_StagingKeepsExtension ensures staging files end in
// .usda/.usdc so usdlint does not reject extension ".staging".
func TestImportUSDPack_StagingKeepsExtension(t *testing.T) {
	dir := t.TempDir()
	prevRoot := ModelPackRoot
	prevStudio := ModelStudioDir
	prevScript := UsdlintScript
	ModelPackRoot = filepath.Join(dir, "usd")
	ModelStudioDir = filepath.Join(dir, "studio")
	// Missing usdlint → unavailable; still must not leave *.staging without real ext.
	UsdlintScript = filepath.Join(dir, "missing.py")
	t.Cleanup(func() {
		ModelPackRoot = prevRoot
		ModelStudioDir = prevStudio
		UsdlintScript = prevScript
	})
	payload := []byte("#usda 1.0\n(\n    defaultPrim = \"Root\"\n)\n\ndef Xform \"Root\"\n{\n}\n")
	_, _ = ImportUSDPack("cdu", "EXTTEST", "EXTTEST.usda", payload, false)
	// No leftover with bare .staging extension
	_ = filepath.Walk(ModelPackRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".staging") && !strings.Contains(filepath.Base(p), ".staging.") {
			t.Errorf("leftover staging without real ext: %s", p)
		}
		return nil
	})
}

// TestImportUSDPack_LintFailNeverPromotes pins PRMT-222: when lint
// rejects (or is unavailable), the final pack path must never appear
// and no .staging residue should remain.
func TestImportUSDPack_LintFailNeverPromotes(t *testing.T) {
	dir := t.TempDir()
	prevRoot := ModelPackRoot
	prevStudio := ModelStudioDir
	prevScript := UsdlintScript
	ModelPackRoot = filepath.Join(dir, "usd")
	ModelStudioDir = filepath.Join(dir, "studio")
	// Force lint unavailable → import must refuse without promoting.
	UsdlintScript = filepath.Join(dir, "missing-usdlint.py")
	t.Cleanup(func() {
		ModelPackRoot = prevRoot
		ModelStudioDir = prevStudio
		UsdlintScript = prevScript
	})

	payload := []byte("#usda 1.0\n(\n    defaultPrim = \"Root\"\n)\n\ndef Xform \"Root\"\n{\n}\n")
	_, err := ImportUSDPack("cdu", "STAGETEST01", "STAGETEST01.usda", payload, false)
	if err == nil {
		t.Fatal("ImportUSDPack: want validation error, got nil")
	}
	if !errors.Is(err, ErrUSDImportValidation) {
		// may wrap unavailable as validation
		if !strings.Contains(err.Error(), "validation") && !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("err = %v, want validation/unavailable", err)
		}
	}

	final := filepath.Join(ModelPackRoot, "cdu", "STAGETEST01.usda")
	if _, err := os.Stat(final); err == nil {
		t.Fatalf("final pack path %s exists after failed import — promote leaked", final)
	}
	stage := final + ".staging"
	if _, err := os.Stat(stage); err == nil {
		t.Fatalf("staging path %s left behind after failed import", stage)
	}
	// packFilePath must still report not found
	if p, err := packFilePath("cdu", "STAGETEST01"); err == nil {
		t.Fatalf("packFilePath found %s after failed import", p)
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
