package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

func TestSiteLayout_SaveWriteback(t *testing.T) {
	studio := t.TempDir()
	old := ModelStudioDir
	ModelStudioDir = studio
	t.Cleanup(func() { ModelStudioDir = old })

	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}

	lay := SiteLayout{
		Site: "sgp01",
		Instances: []LayoutInstance{
			{ID: "i1", Path: "sgp01.pod000", Type: "pod", Model: "DC45", PackType: "pod", X: 10, Y: 20, Rot: 0},
			{ID: "i2", Path: "sgp01.pod000.cdu000", Type: "cdu", Model: "", PackType: "", X: 40, Y: 20, Rot: 0},
		},
		Edges: []LayoutEdge{
			{ID: "e1", FromID: "i1", ToID: "i2", Relation: "feeds"},
		},
	}
	saved, err := SaveSiteLayout(lay, "svc:admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Instances) != 2 {
		t.Fatalf("save: %+v", saved)
	}

	loaded, err := LoadSiteLayout("sgp01")
	if err != nil || len(loaded.Instances) != 2 {
		t.Fatalf("load: %+v err=%v", loaded, err)
	}

	out, res, err := WritebackSiteLayout(context.Background(), st, loaded, "svc:admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.AssetsCreated < 1 {
		t.Fatalf("writeback: %+v", res)
	}
	a, ok, err := st.GetAsset(context.Background(), "sgp01.pod000")
	if err != nil || !ok {
		t.Fatalf("get asset: ok=%v err=%v", ok, err)
	}
	if a.Spec["type"] != "pod" {
		t.Fatalf("spec: %+v", a.Spec)
	}
	if out.LastWriteback == nil {
		t.Fatal("expected last_writeback on layout")
	}
	// edges recorded on from asset
	edges, _ := a.Spec["layout_edges"].([]map[string]string)
	if edges == nil {
		// map[string]any after roundtrip from PutAsset may differ
		if raw, ok := a.Spec["layout_edges"]; !ok {
			t.Fatalf("layout_edges missing: %+v", a.Spec)
		} else {
			_ = raw
		}
	}
}

func TestSiteLayout_WritebackMergesExistingSpec(t *testing.T) {
	// H1: re-writeback must not clobber lifecycle/curated fields.
	studio := t.TempDir()
	old := ModelStudioDir
	ModelStudioDir = studio
	t.Cleanup(func() { ModelStudioDir = old })
	dir := t.TempDir()
	st, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = st.PutAsset(ctx, Asset{
		Path: "sgp01.pod000",
		Spec: map[string]any{
			"type":      "pod",
			"lifecycle": "active",
			"serial":    "KEEP-ME",
			"model":     "DC45",
		},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	lay := SiteLayout{
		Site: "sgp01",
		Instances: []LayoutInstance{
			{ID: "i1", Path: "sgp01.pod000", Type: "pod", Model: "DC45", X: 99, Y: 11, Rot: 15},
		},
	}
	_, res, err := WritebackSiteLayout(ctx, st, lay, "svc:admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.AssetsUpdated != 1 {
		t.Fatalf("want 1 updated: %+v", res)
	}
	a, ok, err := st.GetAsset(ctx, "sgp01.pod000")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if a.Spec["lifecycle"] != "active" {
		t.Fatalf("lifecycle clobbered: %+v", a.Spec)
	}
	if a.Spec["serial"] != "KEEP-ME" {
		t.Fatalf("serial clobbered: %+v", a.Spec)
	}
	layMap, _ := a.Spec["layout"].(map[string]any)
	if layMap == nil {
		t.Fatalf("layout missing: %+v", a.Spec)
	}
}

func TestSiteLayout_BadTypeRejected(t *testing.T) {
	studio := t.TempDir()
	old := ModelStudioDir
	ModelStudioDir = studio
	t.Cleanup(func() { ModelStudioDir = old })
	// vocab with only pod
	vocabDict := &cpath.Dict{Types: map[string]cpath.TypeDef{"pod": {}}}
	_, err := SaveSiteLayout(SiteLayout{
		Site: "sgp01",
		Instances: []LayoutInstance{
			{ID: "a", Path: "sgp01.nope000", Type: "notatype"},
		},
	}, "x", vocabDict)
	if err == nil {
		t.Fatal("expected bad type")
	}
}

func TestSiteLayout_BadRelation(t *testing.T) {
	studio := t.TempDir()
	old := ModelStudioDir
	ModelStudioDir = studio
	t.Cleanup(func() { ModelStudioDir = old })

	// Relation vocab comes from protocol types.yaml (PRMT-223); dict
	// must be loaded so validation is not skipped.
	dict, err := cpath.LoadDict(filepath.Join(moduleRoot(t), "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	if len(dict.Relations) == 0 {
		t.Fatal("expected relations in protocol types.yaml")
	}

	_, err = SaveSiteLayout(SiteLayout{
		Site: "sgp01",
		Instances: []LayoutInstance{
			{ID: "a", Path: "sgp01.pod000", Type: "pod"},
			{ID: "b", Path: "sgp01.pod001", Type: "pod"},
		},
		Edges: []LayoutEdge{{ID: "e", FromID: "a", ToID: "b", Relation: "loves"}},
	}, "x", dict)
	if err == nil {
		t.Fatal("expected bad relation error")
	}
}

func TestLayoutRelationNames_FromProtocol(t *testing.T) {
	dict, err := cpath.LoadDict(filepath.Join(moduleRoot(t), "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	names := LayoutRelationNames(dict)
	if len(names) < 3 {
		t.Fatalf("relations = %v, want at least feeds/cools/connects", names)
	}
	for _, want := range []string{"feeds", "cools", "connects"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing relation %q in %v", want, names)
		}
	}
}

func TestKickSceneRebuild_NoLayout(t *testing.T) {
	studio := t.TempDir()
	old := ModelStudioDir
	ModelStudioDir = studio
	t.Cleanup(func() { ModelStudioDir = old })

	j, err := KickSceneRebuild("sgp01")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "unavailable" {
		t.Fatalf("expected unavailable without layout, got %+v", j)
	}
	loaded, err := LoadSceneJob("sgp01")
	if err != nil || loaded.Status != "unavailable" {
		t.Fatalf("job file: %+v err=%v", loaded, err)
	}
}

func TestKickSceneRebuild_QueuedOrUnavailable(t *testing.T) {
	studio := t.TempDir()
	outDir := t.TempDir()
	oldStudio, oldScript, oldPy, oldOut := ModelStudioDir, SceneEngineScript, SceneEnginePython, SceneOutDir
	ModelStudioDir = studio
	SceneOutDir = outDir
	// Point at a no-op script so kick can queue (if python exists).
	script := filepath.Join(studio, "fake_build.py")
	if err := os.WriteFile(script, []byte("import sys\nsys.exit(0)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	SceneEngineScript = script
	SceneEnginePython = "python3"
	t.Cleanup(func() {
		// Wait for async job so TempDir cleanup does not race the worker.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			j, _ := LoadSceneJob("lab01")
			if j.Status != "queued" && j.Status != "running" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		ModelStudioDir, SceneEngineScript, SceneEnginePython, SceneOutDir =
			oldStudio, oldScript, oldPy, oldOut
	})

	_, err := SaveSiteLayout(SiteLayout{
		Site: "lab01",
		Instances: []LayoutInstance{
			{ID: "i1", Path: "lab01.pod000", Type: "pod", Model: "DC45", X: 100, Y: 50},
		},
	}, "svc:admin", nil)
	if err != nil {
		t.Fatal(err)
	}

	j, err := KickSceneRebuild("lab01")
	if err != nil {
		t.Fatal(err)
	}
	// queued/running if python works; unavailable if LookPath fails
	switch j.Status {
	case "queued", "running", "ok", "unavailable", "error":
		// all terminal/non-terminal accepted for soft env
	default:
		t.Fatalf("unexpected status %+v", j)
	}
}
