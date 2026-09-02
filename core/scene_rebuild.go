// Package core — scene_rebuild.go: async Scene Engine rebuild after Site-Draw (L109 P825).
//
// Soft-fails when python / build.py / layout is missing — writeback still succeeds.
// Job status: artifacts/model-studio/scene-jobs/{site}.json
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SceneEnginePython is the interpreter used to run build.py (default: usdlint venv).
var SceneEnginePython = envOrDefault("CIOS_SCENE_PYTHON",
	envOrDefault("CIOS_USDLINT_PYTHON", "/tmp/usdlint-venv/bin/python3"))

// SceneEngineScript is the scene-engine build entrypoint.
var SceneEngineScript = envOrDefault("CIOS_SCENE_SCRIPT", "tools/scene-engine/build.py")

// SceneOutDir is where {site}.glb + {site}.scene.json are written.
var SceneOutDir = envOrDefault("CIOS_SCENE_OUT", "artifacts/scene")

// SceneJob is one async rebuild attempt for a site.
type SceneJob struct {
	Site       string    `json:"site"`
	JobID      string    `json:"job_id"`
	Status     string    `json:"status"` // queued|running|ok|error|unavailable
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	ExitCode   int       `json:"exit_code,omitempty"`
	OutDir     string    `json:"out_dir,omitempty"`
	LayoutPath string    `json:"layout_path,omitempty"`
	Stdout     string    `json:"stdout,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
	Message    string    `json:"message,omitempty"`
}

var (
	sceneJobMu   sync.Mutex
	sceneRunning = map[string]bool{}
)

func sceneJobPath(site string) string {
	return filepath.Join(ModelStudioDir, "scene-jobs", sanitizeSeg(site)+".json")
}

// LoadSceneJob returns the last job for site, or empty status "none".
func LoadSceneJob(site string) (SceneJob, error) {
	site = sanitizeSeg(site)
	if site == "" {
		return SceneJob{}, fmt.Errorf("core: bad site")
	}
	b, err := os.ReadFile(sceneJobPath(site))
	if err != nil {
		if os.IsNotExist(err) {
			return SceneJob{Site: site, Status: "none"}, nil
		}
		return SceneJob{}, err
	}
	var j SceneJob
	if err := json.Unmarshal(b, &j); err != nil {
		return SceneJob{}, err
	}
	j.Site = site
	return j, nil
}

func saveSceneJob(j SceneJob) error {
	p := sceneJobPath(j.Site)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o644)
}

func truncateRunLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// KickSceneRebuild starts an async build.py run for the site layout.
// Returns immediately with status queued|unavailable. Concurrent kicks for the
// same site are coalesced (returns current running job).
func KickSceneRebuild(site string) (SceneJob, error) {
	site = sanitizeSeg(site)
	if site == "" || !validSiteSlug(site) {
		return SceneJob{}, fmt.Errorf("core: invalid site slug")
	}

	layout := layoutPath(site)
	if _, err := os.Stat(layout); err != nil {
		j := SceneJob{
			Site:       site,
			JobID:      fmt.Sprintf("%s-%d", site, time.Now().UnixNano()),
			Status:     "unavailable",
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
			LayoutPath: layout,
			Message:    "layout file missing — save layout first",
		}
		_ = saveSceneJob(j)
		return j, nil
	}

	py := SceneEnginePython
	script := SceneEngineScript
	if !filepath.IsAbs(script) {
		if wd, e := os.Getwd(); e == nil {
			script = filepath.Join(wd, script)
		}
	}
	if _, err := os.Stat(script); err != nil {
		j := SceneJob{
			Site:       site,
			JobID:      fmt.Sprintf("%s-%d", site, time.Now().UnixNano()),
			Status:     "unavailable",
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
			LayoutPath: layout,
			Message:    "build.py missing: " + script,
		}
		_ = saveSceneJob(j)
		return j, nil
	}
	// Soft-check interpreter (path or PATH lookup).
	if _, err := exec.LookPath(py); err != nil {
		// Also try absolute path that may not be on PATH
		if _, st := os.Stat(py); st != nil {
			j := SceneJob{
				Site:       site,
				JobID:      fmt.Sprintf("%s-%d", site, time.Now().UnixNano()),
				Status:     "unavailable",
				StartedAt:  time.Now().UTC(),
				FinishedAt: time.Now().UTC(),
				LayoutPath: layout,
				Message:    "scene python missing: " + py + " (set CIOS_SCENE_PYTHON)",
			}
			_ = saveSceneJob(j)
			return j, nil
		}
	}

	sceneJobMu.Lock()
	if sceneRunning[site] {
		sceneJobMu.Unlock()
		cur, err := LoadSceneJob(site)
		if err != nil {
			return SceneJob{Site: site, Status: "running", Message: "rebuild already running"}, nil
		}
		if cur.Status == "running" || cur.Status == "queued" {
			return cur, nil
		}
		// stale flag; fall through after re-lock
		sceneJobMu.Lock()
	}
	sceneRunning[site] = true
	sceneJobMu.Unlock()

	outDir := filepath.Join(SceneOutDir, site)
	job := SceneJob{
		Site:       site,
		JobID:      fmt.Sprintf("%s-%d", site, time.Now().UnixNano()),
		Status:     "queued",
		StartedAt:  time.Now().UTC(),
		OutDir:     outDir,
		LayoutPath: layout,
		Message:    "scene rebuild queued",
	}
	if err := saveSceneJob(job); err != nil {
		sceneJobMu.Lock()
		delete(sceneRunning, site)
		sceneJobMu.Unlock()
		return SceneJob{}, err
	}

	go runSceneRebuild(job, py, script)

	return job, nil
}

func runSceneRebuild(job SceneJob, py, script string) {
	defer func() {
		sceneJobMu.Lock()
		delete(sceneRunning, job.Site)
		sceneJobMu.Unlock()
	}()

	job.Status = "running"
	job.Message = "build.py running"
	_ = saveSceneJob(job)

	if err := os.MkdirAll(job.OutDir, 0o755); err != nil {
		job.Status = "error"
		job.FinishedAt = time.Now().UTC()
		job.Message = "mkdir out: " + err.Error()
		_ = saveSceneJob(job)
		return
	}

	args := []string{
		script,
		"-out", job.OutDir,
		"--site", job.Site,
		"--layout", job.LayoutPath,
	}
	// M2: deadline so a hung scene-engine cannot pin the site forever.
	buildCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(buildCtx, py, args...)
	if wd, e := os.Getwd(); e == nil {
		cmd.Dir = wd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	job.Stdout = truncateRunLog(stdout.String(), 4000)
	job.Stderr = truncateRunLog(stderr.String(), 4000)
	job.FinishedAt = time.Now().UTC()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			job.ExitCode = ee.ExitCode()
		} else {
			job.ExitCode = 2
		}
		// Missing deps / import errors → unavailable rather than hard error for ops UX.
		low := strings.ToLower(job.Stderr + " " + runErr.Error())
		if strings.Contains(low, "failed to import") ||
			strings.Contains(low, "no such file") ||
			strings.Contains(low, "not found") {
			job.Status = "unavailable"
			job.Message = "scene engine env incomplete: " + runErr.Error()
		} else {
			job.Status = "error"
			job.Message = "build failed: " + runErr.Error()
		}
		_ = saveSceneJob(job)
		return
	}
	job.ExitCode = 0
	job.Status = "ok"
	job.Message = fmt.Sprintf("wrote %s/{%s.glb,%s.scene.json}", job.OutDir, job.Site, job.Site)
	_ = saveSceneJob(job)
}
