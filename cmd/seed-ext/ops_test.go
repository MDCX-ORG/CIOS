// cmd/seed-ext/ops_test.go — table-driven projection tests for the
// five Project* functions. Per PRMT-165 §5: normal + invalid
// severity + invalid alarm state + invalid ticket state + zero
// opened_at + interval_days<=0 + qty<0.
package main

import (
	"testing"
	"time"
)

func TestProjectAlarm(t *testing.T) {
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		in      SeedAlarm
		wantErr string
	}{
		{
			name: "normal",
			in: SeedAlarm{
				ID:        "al_seed_0001",
				AssetPath: "sgp01.pod000.cdu000",
				Severity:  "major",
				State:     "firing",
				Summary:   "CDU secondary loop ΔT low",
				Since:     since,
			},
		},
		{
			name: "all severities",
			in: SeedAlarm{
				ID:        "al_seed_all",
				AssetPath: "x",
				Severity:  "info",
				State:     "resolved",
				Since:     since,
			},
		},
		{
			name: "empty id",
			in: SeedAlarm{
				AssetPath: "x", Severity: "major", State: "firing", Since: since,
			},
			wantErr: "empty id",
		},
		{
			name: "invalid severity",
			in: SeedAlarm{
				ID: "al_x", AssetPath: "x", Severity: "high", State: "firing", Since: since,
			},
			wantErr: "severity",
		},
		{
			name: "invalid alarm state",
			in: SeedAlarm{
				ID: "al_x", AssetPath: "x", Severity: "major", State: "open", Since: since,
			},
			wantErr: "state",
		},
		{
			name: "zero since",
			in: SeedAlarm{
				ID: "al_x", AssetPath: "x", Severity: "major", State: "firing",
			},
			wantErr: "since is zero",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProjectAlarm(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (got=%+v)", tc.wantErr, got)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Path != tc.in.AssetPath {
				t.Errorf("Path: want %q got %q", tc.in.AssetPath, got.Path)
			}
			if got.ID != tc.in.ID {
				t.Errorf("ID: want %q got %q", tc.in.ID, got.ID)
			}
			if !got.Since.Equal(tc.in.Since) {
				t.Errorf("Since: want %v got %v", tc.in.Since, got.Since)
			}
		})
	}
}

func TestProjectTicket(t *testing.T) {
	opened := time.Date(2026, 6, 30, 22, 11, 0, 0, time.UTC)
	cases := []struct {
		name    string
		in      SeedTicket
		wantErr string
	}{
		{
			name: "normal",
			in: SeedTicket{
				ID:        "tk_seed_0001",
				AlarmID:   "al_seed_0001",
				AssetPath: "sgp01.pod000.cdu000",
				Title:     "CDU ΔT low",
				Severity:  "major",
				State:     "open",
				OpenedAt:  opened,
			},
		},
		{
			name: "all ticket states",
			in: SeedTicket{
				ID: "tk_x", AssetPath: "x", Title: "t", Severity: "info", State: "closed", OpenedAt: opened,
			},
		},
		{
			name: "empty id",
			in: SeedTicket{
				AssetPath: "x", Severity: "major", State: "open", OpenedAt: opened,
			},
			wantErr: "empty id",
		},
		{
			name: "invalid severity",
			in: SeedTicket{
				ID: "tk_x", AssetPath: "x", Severity: "emergency", State: "open", OpenedAt: opened,
			},
			wantErr: "severity",
		},
		{
			name: "invalid ticket state",
			in: SeedTicket{
				ID: "tk_x", AssetPath: "x", Severity: "major", State: "firing", OpenedAt: opened,
			},
			wantErr: "state",
		},
		{
			name: "zero opened_at",
			in: SeedTicket{
				ID: "tk_x", AssetPath: "x", Severity: "major", State: "open",
			},
			wantErr: "opened_at is zero",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProjectTicket(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (got=%+v)", tc.wantErr, got)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.AssetPath != tc.in.AssetPath {
				t.Errorf("AssetPath: want %q got %q", tc.in.AssetPath, got.AssetPath)
			}
			if !got.OpenedAt.Equal(tc.in.OpenedAt) {
				t.Errorf("OpenedAt: want %v got %v", tc.in.OpenedAt, got.OpenedAt)
			}
		})
	}
}

func TestProjectPM(t *testing.T) {
	due := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		in      SeedPM
		wantErr string
	}{
		{
			name: "normal",
			in: SeedPM{
				ID: "pm_seed_0001", AssetPath: "x", Kind: "calendar",
				IntervalDays: 30, NextDue: due, Title: "t", Severity: "minor", Enabled: true,
			},
		},
		{
			name: "empty id",
			in: SeedPM{
				AssetPath: "x", IntervalDays: 30, NextDue: due, Severity: "minor",
			},
			wantErr: "empty id",
		},
		{
			name: "invalid severity",
			in: SeedPM{
				ID: "pm_x", AssetPath: "x", IntervalDays: 30, NextDue: due, Severity: "bad",
			},
			wantErr: "severity",
		},
		{
			name: "interval_days zero",
			in: SeedPM{
				ID: "pm_x", AssetPath: "x", IntervalDays: 0, NextDue: due, Severity: "minor",
			},
			wantErr: "interval_days",
		},
		{
			name: "interval_days negative",
			in: SeedPM{
				ID: "pm_x", AssetPath: "x", IntervalDays: -1, NextDue: due, Severity: "minor",
			},
			wantErr: "interval_days",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProjectPM(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (got=%+v)", tc.wantErr, got)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.IntervalDays != tc.in.IntervalDays {
				t.Errorf("IntervalDays: want %d got %d", tc.in.IntervalDays, got.IntervalDays)
			}
		})
	}
}

func TestProjectSpare(t *testing.T) {
	cases := []struct {
		name    string
		in      SeedSpare
		wantErr string
	}{
		{
			name: "normal",
			in: SeedSpare{
				ID: "sp_seed_0001", SKU: "FILT-CDU-01", Name: "CDU filter",
				Qty: 12, MinQty: 4, Location: "SGP01-store",
			},
		},
		{
			name: "empty id",
			in: SeedSpare{
				SKU: "x", Qty: 1, MinQty: 1,
			},
			wantErr: "empty id",
		},
		{
			name: "qty negative",
			in: SeedSpare{
				ID: "sp_x", SKU: "x", Qty: -1, MinQty: 0,
			},
			wantErr: "qty",
		},
		{
			name: "min_qty negative",
			in: SeedSpare{
				ID: "sp_x", SKU: "x", Qty: 0, MinQty: -1,
			},
			wantErr: "min_qty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProjectSpare(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (got=%+v)", tc.wantErr, got)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.MinQty != tc.in.MinQty {
				t.Errorf("MinQty: want %d got %d", tc.in.MinQty, got.MinQty)
			}
		})
	}
}

func TestProjectInspection(t *testing.T) {
	due := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		in      SeedInspection
		wantErr string
	}{
		{
			name: "normal",
			in: SeedInspection{
				ID: "ins_seed_0001", AssetPath: "x", Title: "Quarterly leak inspection",
				Items:        []string{"check fittings", "check sensor cable"},
				IntervalDays: 90, NextDue: due, Enabled: true,
			},
		},
		{
			name: "empty id",
			in: SeedInspection{
				AssetPath: "x", IntervalDays: 30, NextDue: due,
			},
			wantErr: "empty id",
		},
		{
			name: "interval_days zero",
			in: SeedInspection{
				ID: "ins_x", AssetPath: "x", IntervalDays: 0, NextDue: due,
			},
			wantErr: "interval_days",
		},
		{
			name: "interval_days negative",
			in: SeedInspection{
				ID: "ins_x", AssetPath: "x", IntervalDays: -7, NextDue: due,
			},
			wantErr: "interval_days",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProjectInspection(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (got=%+v)", tc.wantErr, got)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Interval != time.Duration(tc.in.IntervalDays)*24*time.Hour {
				t.Errorf("Interval: want %v got %v", time.Duration(tc.in.IntervalDays)*24*time.Hour, got.Interval)
			}
		})
	}
}
