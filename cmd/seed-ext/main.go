// Command seed-ext seeds core/store assets from a structured EXT-001
// model fixture. It reads cmd/seed-ext/seed/assets.yaml (or the path
// given by -seed), validates each row's type against protocol/types.yaml,
// projects each row to a core.Asset, and writes them into a file-backed
// Store (core.NewFileStore) at -out. No network, no telemetry. dev/test
// only. See PRMT-164.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/yurimeng/cios/core"
	"github.com/yurimeng/cios/pkg/cpath"
)

func main() {
	out := flag.String("out", "", "output store file path (required)")
	seed := flag.String("seed", "cmd/seed-ext/seed/assets.yaml", "input seed assets.yaml path")
	ops := flag.String("ops", "cmd/seed-ext/seed/ops.yaml", "input seed ops.yaml path (empty to skip; PRMT-165)")
	protocolDir := flag.String("protocol", "./protocol", "protocol/ directory for dictionary load")
	flag.Parse()

	if *out == "" {
		log.Fatalf("seed-ext: -out is required")
	}

	knownTypes, err := loadKnownTypes(*protocolDir)
	if err != nil {
		log.Fatalf("seed-ext: load known types: %v", err)
	}

	doc, err := loadSeedDoc(*seed)
	if err != nil {
		log.Fatalf("seed-ext: load seed %s: %v", *seed, err)
	}
	if len(doc.Assets) == 0 {
		log.Fatalf("seed-ext: no assets in %s", *seed)
	}

	store, err := core.NewFileStore(*out)
	if err != nil {
		log.Fatalf("seed-ext: open store %s: %v", *out, err)
	}

	ctx := context.Background()
	for i, sa := range doc.Assets {
		a, err := Project(sa, knownTypes)
		if err != nil {
			log.Fatalf("seed-ext: row %d (%s): %v", i+1, sa.Path, err)
		}
		if _, err := store.PutAsset(ctx, a, 0); err != nil {
			log.Fatalf("seed-ext: PutAsset %s: %v", a.Path, err)
		}
	}

	fmt.Printf("seeded %d assets → %s\n", len(doc.Assets), *out)

	// PRMT-165: optional post-assets ops seed. -ops="" preserves the
	// PRMT-164 asset-only behavior.
	if *ops != "" {
		nA, nT, nP, nS, nI, err := seedOps(ctx, store, *ops)
		if err != nil {
			log.Fatalf("seed-ext: ops %s: %v", *ops, err)
		}
		fmt.Printf("seeded ops → %s (alarms=%d tickets=%d pm=%d spares=%d inspections=%d)\n",
			*out, nA, nT, nP, nS, nI)
	}
}

// seedOps loads OpsDoc from path, projects each row, and writes
// through the existing Store upsert methods (PRMT-165 §2). Returns
// per-section counts. Fails loudly on any projection or write error.
func seedOps(ctx context.Context, store core.Store, path string) (nA, nT, nP, nS, nI int, err error) {
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		return 0, 0, 0, 0, 0, rerr
	}
	var doc OpsDoc
	if yerr := yaml.Unmarshal(raw, &doc); yerr != nil {
		return 0, 0, 0, 0, 0, yerr
	}

	alarms := make([]core.Alarm, 0, len(doc.Alarms))
	for _, sa := range doc.Alarms {
		a, perr := ProjectAlarm(sa)
		if perr != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("alarm %s: %w", sa.ID, perr)
		}
		alarms = append(alarms, a)
	}
	if len(alarms) > 0 {
		if serr := store.SeedAlarms(ctx, alarms); serr != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("SeedAlarms: %w", serr)
		}
		nA = len(alarms)
	}

	for _, st := range doc.Tickets {
		t, perr := ProjectTicket(st)
		if perr != nil {
			return nA, nT, nP, nS, nI, fmt.Errorf("ticket %s: %w", st.ID, perr)
		}
		if _, perr := store.PutTicket(ctx, t, 0); perr != nil {
			return nA, nT, nP, nS, nI, fmt.Errorf("PutTicket %s: %w", st.ID, perr)
		}
		nT++
	}

	for _, sp := range doc.PMSchedules {
		p, perr := ProjectPM(sp)
		if perr != nil {
			return nA, nT, nP, nS, nI, fmt.Errorf("pm %s: %w", sp.ID, perr)
		}
		if perr := store.PutPMSchedule(ctx, p); perr != nil {
			return nA, nT, nP, nS, nI, fmt.Errorf("PutPMSchedule %s: %w", sp.ID, perr)
		}
		nP++
	}

	for _, ss := range doc.Spares {
		sp, perr := ProjectSpare(ss)
		if perr != nil {
			return nA, nT, nP, nS, nI, fmt.Errorf("spare %s: %w", ss.ID, perr)
		}
		if perr := store.PutSpare(ctx, sp); perr != nil {
			return nA, nT, nP, nS, nI, fmt.Errorf("PutSpare %s: %w", ss.ID, perr)
		}
		nS++
	}

	for _, si := range doc.Inspections {
		it, perr := ProjectInspection(si)
		if perr != nil {
			return nA, nT, nP, nS, nI, fmt.Errorf("inspection %s: %w", si.ID, perr)
		}
		if perr := store.PutInspectionTemplate(ctx, it); perr != nil {
			return nA, nT, nP, nS, nI, fmt.Errorf("PutInspectionTemplate %s: %w", si.ID, perr)
		}
		nI++
	}

	return nA, nT, nP, nS, nI, nil
}

// loadKnownTypes returns the set of top-level type keys from
// protocol/types.yaml (plus any ext.d fragments, per L54).
func loadKnownTypes(protocolDir string) (map[string]struct{}, error) {
	d, err := cpath.LoadDict(protocolDir)
	if err != nil {
		return nil, err
	}
	known := make(map[string]struct{}, len(d.Types))
	for k := range d.Types {
		known[k] = struct{}{}
	}
	return known, nil
}

func loadSeedDoc(path string) (SeedDoc, error) {
	var doc SeedDoc
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc, err
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return doc, err
	}
	return doc, nil
}
