// cmd/seed-ext/ops.go — pure projection from seed/ops.yaml to the
// five spec-008/spec-003-shaped core types. No I/O. Per PRMT-165 §4.
//
// All five sections use YAML-tagged mirror structs (Seed*) so the
// on-disk keys are uniform snake_case and self-documenting. core.Alarm
// and core.SparePart carry only json tags, so yaml.v3 would bind
// tag-less by the lowercased Go field name — e.g. asset_path/min_qty
// would silently NOT bind. The Seed* mirrors remove that footgun.
package main

import (
	"fmt"
	"time"

	"github.com/yurimeng/cios/core"
)

// OpsDoc is the on-disk shape of seed/ops.yaml.
type OpsDoc struct {
	Alarms      []SeedAlarm      `yaml:"alarms"`
	Tickets     []SeedTicket     `yaml:"tickets"`
	PMSchedules []SeedPM         `yaml:"pm_schedules"`
	Spares      []SeedSpare      `yaml:"spares"`
	Inspections []SeedInspection `yaml:"inspections"`
}

// SeedAlarm mirrors the YAML shape of one ops.yaml alarm row.
type SeedAlarm struct {
	ID        string    `yaml:"id"`
	AssetPath string    `yaml:"asset_path"`
	Severity  string    `yaml:"severity"`
	State     string    `yaml:"state"`
	Summary   string    `yaml:"summary"`
	Since     time.Time `yaml:"since"`
}

// SeedTicket mirrors the YAML shape of one ops.yaml ticket row.
type SeedTicket struct {
	ID        string     `yaml:"id"`
	AlarmID   string     `yaml:"alarm_id,omitempty"`
	AssetPath string     `yaml:"asset_path"`
	Title     string     `yaml:"title"`
	Severity  string     `yaml:"severity"`
	State     string     `yaml:"state"`
	OpenedAt  time.Time  `yaml:"opened_at"`
	ClosedAt  *time.Time `yaml:"closed_at,omitempty"`
}

// SeedPM mirrors the YAML shape of one ops.yaml pm_schedules row.
type SeedPM struct {
	ID           string    `yaml:"id"`
	AssetPath    string    `yaml:"asset_path"`
	Kind         string    `yaml:"kind"`
	IntervalDays int       `yaml:"interval_days"`
	NextDue      time.Time `yaml:"next_due"`
	Title        string    `yaml:"title"`
	Severity     string    `yaml:"severity"`
	Enabled      bool      `yaml:"enabled"`
}

// SeedSpare mirrors the YAML shape of one ops.yaml spares row.
type SeedSpare struct {
	ID       string `yaml:"id"`
	SKU      string `yaml:"sku"`
	Name     string `yaml:"name"`
	Qty      int    `yaml:"qty"`
	MinQty   int    `yaml:"min_qty"`
	Location string `yaml:"location"`
}

// SeedInspection mirrors the YAML shape of one ops.yaml inspections row.
type SeedInspection struct {
	ID           string    `yaml:"id"`
	AssetPath    string    `yaml:"asset_path"`
	Title        string    `yaml:"title"`
	Items        []string  `yaml:"items"`
	IntervalDays int       `yaml:"interval_days"`
	NextDue      time.Time `yaml:"next_due"`
	Enabled      bool      `yaml:"enabled"`
}

// Severity set per spec-003 §2 (critical|major|minor|info).
var validSeverities = map[string]struct{}{
	"critical": {},
	"major":    {},
	"minor":    {},
	"info":     {},
}

// Alarm state set per core.Alarm comment (firing|acked|resolved).
var validAlarmStates = map[string]struct{}{
	"firing":   {},
	"acked":    {},
	"resolved": {},
}

// Ticket state set per spec-008 §2 (open|acknowledged|resolved|closed).
var validTicketStates = map[string]struct{}{
	"open":         {},
	"acknowledged": {},
	"resolved":     {},
	"closed":       {},
}

func isValidSeverity(s string) bool {
	_, ok := validSeverities[s]
	return ok
}

// ProjectAlarm validates severity ∈ {critical,major,minor,info} and
// state ∈ {firing,acked,resolved} (core.Alarm state set), maps
// asset_path → Alarm.Path and since → time.Time; returns error
// otherwise.
func ProjectAlarm(s SeedAlarm) (core.Alarm, error) {
	if s.ID == "" {
		return core.Alarm{}, fmt.Errorf("alarm: empty id")
	}
	if !isValidSeverity(s.Severity) {
		return core.Alarm{}, fmt.Errorf("alarm %s: severity %q not in {critical,major,minor,info}", s.ID, s.Severity)
	}
	if _, ok := validAlarmStates[s.State]; !ok {
		return core.Alarm{}, fmt.Errorf("alarm %s: state %q not in {firing,acked,resolved}", s.ID, s.State)
	}
	if s.Since.IsZero() {
		return core.Alarm{}, fmt.Errorf("alarm %s: since is zero", s.ID)
	}
	return core.Alarm{
		ID:       s.ID,
		Path:     s.AssetPath,
		Severity: s.Severity,
		State:    s.State,
		Summary:  s.Summary,
		Since:    s.Since,
	}, nil
}

// ProjectTicket validates severity ∈ {critical,major,minor,info} and
// state ∈ {open,acknowledged,resolved,closed} and opened_at != zero.
func ProjectTicket(s SeedTicket) (core.Ticket, error) {
	if s.ID == "" {
		return core.Ticket{}, fmt.Errorf("ticket: empty id")
	}
	if !isValidSeverity(s.Severity) {
		return core.Ticket{}, fmt.Errorf("ticket %s: severity %q not in {critical,major,minor,info}", s.ID, s.Severity)
	}
	if _, ok := validTicketStates[s.State]; !ok {
		return core.Ticket{}, fmt.Errorf("ticket %s: state %q not in {open,acknowledged,resolved,closed}", s.ID, s.State)
	}
	if s.OpenedAt.IsZero() {
		return core.Ticket{}, fmt.Errorf("ticket %s: opened_at is zero", s.ID)
	}
	var closed *time.Time
	if s.ClosedAt != nil && !s.ClosedAt.IsZero() {
		t := s.ClosedAt.UTC()
		closed = &t
	} else if s.State == "closed" {
		// Default closed_at = opened_at when seed omits it (KB cases need a stamp).
		t := s.OpenedAt.UTC()
		closed = &t
	}
	return core.Ticket{
		ID:         s.ID,
		AlarmID:    s.AlarmID,
		AssetPath:  s.AssetPath,
		Title:      s.Title,
		Severity:   s.Severity,
		State:      s.State,
		OpenedAt:   s.OpenedAt,
		Assignee:   "",
		AckedAt:    nil,
		ResolvedAt: nil,
		ClosedAt:   closed,
	}, nil
}

// ProjectPM validates severity ∈ {critical,major,minor,info} and
// interval_days > 0 (mirror ticket severity set; spec-008 §2 PM).
func ProjectPM(s SeedPM) (core.PMSchedule, error) {
	if s.ID == "" {
		return core.PMSchedule{}, fmt.Errorf("pm: empty id")
	}
	if !isValidSeverity(s.Severity) {
		return core.PMSchedule{}, fmt.Errorf("pm %s: severity %q not in {critical,major,minor,info}", s.ID, s.Severity)
	}
	if s.IntervalDays <= 0 {
		return core.PMSchedule{}, fmt.Errorf("pm %s: interval_days %d must be > 0", s.ID, s.IntervalDays)
	}
	if s.NextDue.IsZero() {
		return core.PMSchedule{}, fmt.Errorf("pm %s: next_due is zero", s.ID)
	}
	return core.PMSchedule{
		ID:           s.ID,
		AssetPath:    s.AssetPath,
		Kind:         s.Kind,
		IntervalDays: s.IntervalDays,
		LastRun:      nil,
		NextDue:      s.NextDue,
		Title:        s.Title,
		Severity:     s.Severity,
		Enabled:      s.Enabled,
	}, nil
}

// ProjectSpare validates qty ≥ 0 and min_qty ≥ 0.
func ProjectSpare(s SeedSpare) (core.SparePart, error) {
	if s.ID == "" {
		return core.SparePart{}, fmt.Errorf("spare: empty id")
	}
	if s.Qty < 0 {
		return core.SparePart{}, fmt.Errorf("spare %s: qty %d must be >= 0", s.ID, s.Qty)
	}
	if s.MinQty < 0 {
		return core.SparePart{}, fmt.Errorf("spare %s: min_qty %d must be >= 0", s.ID, s.MinQty)
	}
	return core.SparePart{
		ID:       s.ID,
		SKU:      s.SKU,
		Name:     s.Name,
		Qty:      s.Qty,
		MinQty:   s.MinQty,
		Location: s.Location,
	}, nil
}

// ProjectInspection validates interval_days > 0.
func ProjectInspection(s SeedInspection) (core.InspectionTemplate, error) {
	if s.ID == "" {
		return core.InspectionTemplate{}, fmt.Errorf("inspection: empty id")
	}
	if s.IntervalDays <= 0 {
		return core.InspectionTemplate{}, fmt.Errorf("inspection %s: interval_days %d must be > 0", s.ID, s.IntervalDays)
	}
	if s.NextDue.IsZero() {
		return core.InspectionTemplate{}, fmt.Errorf("inspection %s: next_due is zero", s.ID)
	}
	return core.InspectionTemplate{
		ID:        s.ID,
		AssetPath: s.AssetPath,
		Title:     s.Title,
		Items:     s.Items,
		Interval:  time.Duration(s.IntervalDays) * 24 * time.Hour,
		NextDue:   s.NextDue,
		Enabled:   s.Enabled,
	}, nil
}
