# policy/rego/context.rego — Context-only PDP sample (PRMT-104;
# org/site context: PRMT-110).
#
# SCOPE (spec-009 §7.1; PRMT-104 §1, §6): this package is a
# CONTEXTUAL policy decision point. It judges realm/action/MFA/
# time only. Resource-scope authority (which asset branches a
# caller may touch) is the exclusive authority of core /v1's
# existing rules, enforced by PRMT-105. This file MUST NOT contain
# any matching logic on the path field — the acceptance grep in
# PRMT-104 §7 pins that.
#
# PRMT-110 §1, §4: this file also judges the cross-site switching
# context (TargetSite ∈ Sites) so the R6 site switcher can deny
# an identity that tries to leave its reachable site set. The
# rule is STRICTLY contextual: it does not look at resource
# sub-trees (L81 red line). PRMT-110 §5: a missing or empty
# Org / Sites claim must fail closed; the rule below enforces
# that by making TargetSite a hard constraint whenever the
# caller asserts a non-empty TargetSite.
#
# Default verdict: deny. Every rule below only narrows the allow
# surface; nothing returns true unconditionally. This is the
# fail-closed property the gateway middleware relies on.

package cios.authz

# Default deny. PRMT-104 §5: fail-closed; the sample must never
# widen beyond an explicit rule.
default allow = false

# Sensitive actions require MFA. Without MFA, the rule refuses
# regardless of realm.
sensitive_actions := {"write"}

# org_or_site_context_active is true when the request asserts a
# site target that needs to be checked against the identity's
# reachable site set. PRMT-110 §4: empty TargetSite means "no
# site switch requested" and the rule does not constrain — that
# keeps global / pre-site reads working. A non-empty TargetSite
# makes the org/site rule a hard gate.
org_or_site_context_active if {
	input.target_site != ""
}

# site_allowed: TargetSite is in the identity's reachable site
# set. PRMT-110 §4, §5: fail-closed. Whitespace-only TargetSite
# or an empty Sites list both deny.
site_allowed if {
	input.target_site != ""
	some i
	input.sites[i] == input.target_site
}

allow if {
	input.action in sensitive_actions
	input.mfa == true
	not org_or_site_context_active
}

# Read is allowed for any verified realm. We rely on the gateway
# (PRMT-101..103) to have already verified the token; this PDP
# does NOT inspect token scope to grant access — that is core
# RBAC's job (L34/L50).
allow if {
	input.action == "read"
	input.realm != ""
	not org_or_site_context_active
}

# Off-hours write is refused even with MFA. The window is a
# stand-in for the kind of time/context rule a future PRMT will
# refine; pinning it here keeps the time hook observable in
# tests.
allow if {
	input.action == "write"
	input.mfa == true
	not off_hours(input.time)
	not org_or_site_context_active
}

# Site-switched reads: an identity with a verified token and a
# target site that is in its reachable set may read. PRMT-110
# §1 (R6 site switcher). Resource scope inside the site is still
# core RBAC (L81 red line) — this rule never touches path /
# resource identifiers.
allow if {
	input.action == "read"
	input.realm != ""
	input.org != ""
	site_allowed
}

# Site-switched writes: MFA + in-set TargetSite + not off-hours.
# Same fail-closed discipline as the global write rule.
allow if {
	input.action == "write"
	input.mfa == true
	input.org != ""
	site_allowed
	not off_hours(input.time)
}

off_hours(t) if {
	h := number_format(time_hour(t))
	h < 8
}

off_hours(t) if {
	h := number_format(time_hour(t))
	h >= 20
}