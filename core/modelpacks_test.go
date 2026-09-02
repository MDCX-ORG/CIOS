package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yurimeng/cios/pkg/cpath"
)

func TestModelPacks_ListBindingsLintSoft(t *testing.T) {
	root := t.TempDir()
	studio := t.TempDir()
	oldRoot, oldStudio, oldPy, oldScript := ModelPackRoot, ModelStudioDir, UsdlintPython, UsdlintScript
	ModelPackRoot, ModelStudioDir = root, studio
	// Force lint unavailable without a real usdlint env.
	UsdlintPython, UsdlintScript = "python3", filepath.Join(root, "missing-usdlint.py")
	t.Cleanup(func() {
		ModelPackRoot, ModelStudioDir = oldRoot, oldStudio
		UsdlintPython, UsdlintScript = oldPy, oldScript
	})

	// G-layer fixture
	dir := filepath.Join(root, "pod")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(dir, "DC45.usdc")
	if err := os.WriteFile(pack, []byte("not-real-usd"), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := ListModelPacks()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Model != "DC45" {
		t.Fatalf("list: %+v", items)
	}

	doc, err := SaveBindings("pod", "DC45", "svc:test", "note", []ModelBinding{
		{Prim: "geo/pump000", CiosType: "pump", CiosRelpath: "fws.flow"},
		{Prim: "", CiosType: "skip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Bindings) != 1 {
		t.Fatalf("bindings clean: %+v", doc.Bindings)
	}

	rep, err := RunModelPackLint("pod", "DC45")
	if err != nil {
		t.Fatal(err)
	}
	if rep.SoftStatus != "lint_unavailable" && rep.Result != "unavailable" {
		// exit 2 path or missing script
		if rep.ExitCode == 0 {
			t.Fatalf("expected soft unavailable lint, got %+v", rep)
		}
	}

	sum, bdoc, _, err := GetModelPackDetail("pod", "DC45")
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasBindings || sum.BindingCount != 1 {
		t.Fatalf("detail bindings: %+v", sum)
	}
	if len(bdoc.Bindings) != 1 {
		t.Fatalf("detail doc: %+v", bdoc)
	}
}

func TestG8Conform_SFirst(t *testing.T) {
	root := t.TempDir()
	studio := t.TempDir()
	oldRoot, oldStudio := ModelPackRoot, ModelStudioDir
	ModelPackRoot, ModelStudioDir = root, studio
	t.Cleanup(func() { ModelPackRoot, ModelStudioDir = oldRoot, oldStudio })

	dir := filepath.Join(root, "pod")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "X.usdc"), []byte("x"), 0o644)

	// Bad type in bindings (picv → valve via alias map)
	_, err := SaveBindings("pod", "X", "svc:t", "", []ModelBinding{
		{Prim: "/root/geo/picv000", CiosType: "picv"},
		{Prim: "/root/geo/pump000", CiosType: "pump"}, // already in vocab
	})
	if err != nil {
		t.Fatal(err)
	}

	dict, err := cpath.LoadDict("protocol")
	if err != nil {
		dict, err = cpath.LoadDict(filepath.Join("..", "protocol"))
	}
	if err != nil {
		t.Skipf("protocol dict: %v", err)
	}

	// dry-run
	rep, err := RunG8Conform(dict, "pod", "X", "svc:admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DryRun != true || rep.Mode != "g8" {
		t.Fatalf("report: %+v", rep)
	}
	found := false
	for _, p := range rep.Proposals {
		if p.Prim == "/root/geo/picv000" && p.ToType == "valve" && p.Action == "apply" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected picv→valve proposal, got %+v", rep.Proposals)
	}

	// apply
	rep2, err := RunG8Conform(dict, "pod", "X", "svc:admin", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.AppliedCount < 1 {
		t.Fatalf("applied: %+v", rep2)
	}
	doc, ok, err := loadBindings("pod", "X")
	if err != nil || !ok {
		t.Fatalf("load bindings: ok=%v err=%v", ok, err)
	}
	var got string
	for _, b := range doc.Bindings {
		if b.Prim == "/root/geo/picv000" {
			got = b.CiosType
		}
	}
	if got != "valve" {
		t.Fatalf("cios_type after conform = %q, want valve", got)
	}
	if rep2.GLayerAction != "none" {
		t.Fatalf("must not write G-layer, got %s", rep2.GLayerAction)
	}
}
