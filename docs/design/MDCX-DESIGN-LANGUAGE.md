# MDCX Design Language — CIOS console binding

Version 2.0 · 2026-08-31 · Authority: `../MDC/docs/MDCX-DESIGN-LANGUAGE.md` (v2.0) + `../MDC/docs/THEME.md` + `../MDC/design-system/tokens.css` (sole source of color values). **This file = the CIOS console binding of that language.**
Companion: `MDCX-DESIGN-LANGUAGE.html` (v1.0 self-demo, superseded — see banner in that file).

Scope: the CIOS product portals (ops-portal, customer-portal, /admin). The MDC marketing site (Stage) is bound in the MDC repo; the two surface classes share one language.

Reading rule: **color values are never authored here.** A value in this document is a transcription of `../MDC/design-system/tokens.css` into the shadcn HSL-triplet form that `web/packages/ui/src/styles/globals.css` consumes. If the two ever disagree, upstream wins and this file is wrong.

---

## 0. Decision record

Three conflicts existed between the MDC site system (`design-system/tokens.css`, "commercial mono") and the CIOS UI guideline v1.0 (shadcn tokens). Resolved as follows — **all three resolutions remain in force under v2.0**:

| # | Conflict | Resolution | Rationale |
|---|----------|-----------|-----------|
| D1 | MDC = strictly monochrome ("never a third hue"); new MDCX logo carries a blue X-stroke | **Mono base + exactly one accent: X Blue.** The mono doctrine survives; blue is added as a *rationed* signal color, used the way the logo uses it — one stroke, not a wash. | The logo is the brand contract. A single accent also matches the current reference class (x.com, Tesla, NVIDIA, Vercel: mono chrome + one signal). |
| D2 | Radius: MDC 2px sharp vs CIOS 8px rounded | **By surface class: Stage = 2px, Console = 6px.** One geometry *principle* (chamfered, near-sharp — echoing the logo letterforms), two densities. 8px is retired. | Marketing needs the industrial sharpness of the hardware; an operator console needs slightly softer geometry for dense forms. 6px keeps CIOS's existing `calc(radius − 2px)` button math working (`--radius: 8px → 6px` inner). |
| D3 | Token format: hex `--mdc-*` vs shadcn HSL channels | **Canonical values live upstream (hex); each codebase keeps its native binding.** MDC site keeps `--mdc-*`, CIOS keeps shadcn variable names (values updated). No cross-codebase renames. | Minimum change that solves the problem. Renaming shadcn vars would break the Tailwind preset; renaming `--mdc-*` would touch every page. |

**D2 note (anti-drift):** the console's `--radius: 0.375rem` (6px) diverging from Stage's 2px is the **deliberate D2 resolution**, not drift. Do not "harmonize" it.

**D3 note (v2.0):** v1.0 read "canonical values live here (hex + HSL given for every color)". v2.0 moves canonicality upstream to `tokens.css`; this file transcribes. The binding *mechanism* is unchanged — shadcn HSL triplets, same variable names, Tailwind wraps `hsl()`. No `--mdc-*` parallel token system in CIOS.

Brand naming: the operating brand is **MDCX**. Hardware blocks (DC45, AC40/AC45, A32) and software (CIOS) are product names under MDCX.

---

## 0.1 Changelog — what v2.0 fixes

v1.0 (2026-07-17) shipped four contrast/consistency defects that the CIOS console was live on until PRMT-239:

| # | Defect (v1.0) | Measured | Fixed by v2.0 |
|---|---|---|---|
| 1 | **Day surface ramp non-monotonic** — `#FFF → #FAFAFA → #F4F4F4 → #FFF`, so component elevation inverted on theme switch | — | Elevation ramp is monotonic in both themes: Day `#E6E9EE → #F5F7F9 → #FAFBFC → #FFFFFF` |
| 2 | **Accent crossed hue families on switch** — 217° (dark) → 224° (light) | Δ7° hue jump | Hue locked to 215–219°: `#2F6FE0` Night / `#1560CC` Day |
| 3 | **warn on Day canvas illegible** — `#EAB308` on `#FAFAFA` | **1.84:1** (AA needs 4.5:1) | `#8A5A00` on `#F5F7F9` = **5.52:1** |
| 4 | **CIOS-local: form controls invisible** — `--input: 0 0% 90%` (`#E5E5E5`) on Day canvas `#FAFAFA` | **1.21:1** (WCAG 1.4.11 needs 3:1) | `line-input` `#7E8899` on `#F5F7F9` = **3.24:1** |

v2.0 also splits the accent into two roles (fill vs text) — see §3.1 — because v1.0 used one value for both and the Night CTA label ran at 3.26:1.

---

## 1. Brand foundation

### 1.1 Logo

Assets: `MDC/LOGO/MDCX Light.png` (dark mark, light backgrounds) · `MDC/LOGO/MDC DARK.png` (white mark, dark backgrounds). The wordmark is an angular, chamfered technical letterform; the upper-right stroke of the X is X Blue. This stroke is the origin of the entire accent system.

Rules:

- Use the supplied files only. Never recolor, outline, shadow, gradient, or skew the mark. Never set the whole mark in blue.
- Clearspace: minimum padding on all sides = height of the "M". Minimum render width: 96px (web), 24mm (print).
- On dark surfaces use the white mark; on light surfaces use the dark mark. Never place either on mid-grey or imagery without a solid backing surface.
- The X-stroke blue in the asset is authoritative; UI accent tokens (§3.1) are tuned per theme for contrast and do not need to match the PNG pixel-for-pixel.
- Do not typeset "MDCX" in body copy with letterform styling — plain text, all caps.

### 1.2 The X principle (accent rationing)

The logo uses blue exactly once. Interfaces do the same:

> **Per view region, at most one X Blue moment.** The primary CTA on a marketing section, the active nav item in the console, the selected node on a canvas. Everything else is mono ink.

X Blue is never: body text, borders-at-rest, backgrounds larger than a chip, decoration, or a status color. Status has its own quartet (§3.1) reserved for machine truth — alarms, health, telemetry.

---

## 2. Principles

1. **Mono base, one signal.** Near-black and near-white carry the brand; X Blue is rationed per the X principle. If everything is highlighted, nothing is.
2. **Two surfaces, one language.** *Stage* (marketing/investor pages): dark, cinematic, editorial, 2px geometry. *Console* (CIOS portals): light-default + dark, dense, calm, 6px geometry. Same palette, type, voice, and status system on both.
3. **Engineering truth.** Real numbers beat adjectives ("1,240 kW", "≥7 days offline" — not "powerful", "reliable"). Identifiers are monospace, always (`sgp01.pod002.cdu000.fws.supply.flow`). State is honest: errors show the real upstream message; roadmap items are labeled Roadmap; async work shows its lifecycle, never a bare spinner.
4. **Token-only color.** Every color on every surface resolves to a token bound in §3.1. Raw palette classes and hex literals in page code are forbidden on both codebases.
5. **Dense but calm.** Consoles prefer one screen of high-density truth (tables, pills, mono ids) over whitespace-heavy cards; density comes from a consistent scale, not ad-hoc shrinking.
6. **Sharp geometry.** Chamfered, near-sharp corners echo the letterforms of the wordmark. Nothing pill-shaped except status pills; no blobs, no large radii, no glassmorphism.

---

## 3. Color — CIOS binding of Theme v2.0

One cool-neutral scale (hue **210–220°**, sat ≤6%) plus one accent hue (**215–219°**). Nothing else. No warm grey, no second blue, no third hue anywhere in chrome. Pure `#000`/`#FFF` are reserved (on-media type; `brand-pressed` in Day) — large fields use near-black `#0C0E12` / near-white `#F5F7F9` instead, which avoids OLED halation and the "dirty grey" cast neutral greys pick up next to a blue accent.

`surface-0…4` is **elevation, not luminance**: 0 = sunken, 4 = floating, monotonic in both themes.

### 3.1 Bound tokens

Every row below is live in `web/packages/ui/src/styles/globals.css` (`:root` = Day, `.dark` = Night) and reachable from Tailwind via `@cios/ui/tailwind.preset.cjs`.

| shadcn variable | v2.0 role | Day (`:root`) | Night (`.dark`) |
|---|---|---|---|
| `--background` | surface-1 · canvas | `210 20% 97%` `#F5F7F9` | `220 20% 6%` `#0C0E12` |
| `--foreground` | ink-primary | `218 18% 9%` `#12151A` | `210 25% 98%` `#FAFBFC` |
| `--card` | surface-3 · card, panel | `0 0% 100%` `#FFFFFF` | `219 15% 11%` `#171A20` |
| `--card-foreground` | ink-primary | `218 18% 9%` `#12151A` | `210 25% 98%` `#FAFBFC` |
| `--popover` | surface-4 · popover, dialog | `0 0% 100%` `#FFFFFF` | `218 16% 14%` `#1E222A` |
| `--popover-foreground` | ink-primary | `218 18% 9%` `#12151A` | `210 25% 98%` `#FAFBFC` |
| `--primary` | brand (invert-mono, **not** X Blue) | `218 18% 9%` `#12151A` | `210 25% 98%` `#FAFBFC` |
| `--primary-foreground` | brand-ink | `210 25% 98%` `#FAFBFC` | `220 20% 6%` `#0C0E12` |
| `--secondary` | surface-2 · section band, table head | `210 20% 98%` `#FAFBFC` | `218 19% 8%` `#111419` |
| `--secondary-foreground` | ink-primary | `218 18% 9%` `#12151A` | `210 25% 98%` `#FAFBFC` |
| `--muted` | surface-2 | `210 20% 98%` `#FAFBFC` | `218 19% 8%` `#111419` |
| `--muted-foreground` | ink-tertiary | `218 11% 38%` `#565E6C` | `219 11% 64%` `#9AA1AE` |
| `--accent` | **accent fill** — CTA bg, active chip, hover fill | `215 81% 44%` `#1560CC` | `218 74% 53%` `#2F6FE0` |
| `--accent-foreground` | accent-ink (label on fill) | `0 0% 100%` `#FFFFFF` | `0 0% 100%` `#FFFFFF` |
| `--accent-text` | **accent text** — links, active nav | `215 81% 44%` `#1560CC` | `217 91% 70%` `#6BA1F8` |
| `--destructive` | critical | `3 72% 44%` `#C0261F` | `0 91% 71%` `#F87171` |
| `--destructive-foreground` | on critical fill | `0 0% 100%` `#FFFFFF` | `220 20% 6%` `#0C0E12` |
| `--success` | ok | `153 91% 25%` `#067A46` | `158 64% 52%` `#34D399` |
| `--success-foreground` | on ok fill | `0 0% 100%` `#FFFFFF` | `220 20% 6%` `#0C0E12` |
| `--warning` | warn | `39 100% 27%` `#8A5A00` | `43 96% 56%` `#FBBF24` |
| `--warning-foreground` | on warn fill | `0 0% 100%` `#FFFFFF` | `220 20% 6%` `#0C0E12` |
| `--info` | info | `203 88% 35%` `#0B6BA8` | `198 93% 60%` `#38BDF8` |
| `--info-foreground` | on info fill | `0 0% 100%` `#FFFFFF` | `220 20% 6%` `#0C0E12` |
| `--border` | line (solid, no alpha) | `216 17% 88%` `#DCE0E6` | `217 16% 16%` `#232830` |
| `--input` | **line-input** (form controls, ≥3:1) | `218 12% 55%` `#7E8899` | `218 11% 40%` `#5C6472` |
| `--ring` | accent-ring (2px focus ring) | `215 81% 44%` `#1560CC` | `217 90% 63%` `#4C8DF6` |
| `--radius` | console geometry (D2) | `0.375rem` | `0.375rem` |
| `--font-sans` / `--font-mono` | type stacks (unchanged in v2.0) | Inter / ui-monospace | Inter / ui-monospace |

HSL triplets are integers (upstream publishes its surface bindings the same way); the ≤1/255 per-channel rounding drift this introduces is accepted. The contrast contract (§3.4) is asserted against the canonical **hex**, and every pair clears its threshold by ≥0.2.

**Status-foreground direction is a CIOS binding decision** (PRMT-221, re-affirmed by PRMT-239; upstream does not specify status foregrounds): Night status colors are bright, so their foreground is near-black `#0C0E12` (white would run ~2.5:1); Day status colors are dark, so their foreground is white (5.0–5.9:1). `--destructive-foreground` on Night changed from white to `#0C0E12` in v2.0 for exactly this reason.

**Accent fill vs accent text.** `--accent` is a *fill* role: CTA backgrounds, active chips, `hover:bg-accent`, and the 2px `border-accent` rule beside an active nav item. `--accent-text` is the *text* role: links and active-nav labels (`text-accent-text`). Never use the fill value as body-sized text — that is the v1.0 3.26:1 defect.

### 3.2 Unbound upstream roles

These exist in `../MDC/design-system/tokens.css` but have **no CIOS binding** because the console has no consumption point today. shadcn carries interaction state via Tailwind opacity/hover utilities instead — the mechanism is deliberately not swapped. Binding any of them requires a new PRMT.

| Upstream role | Why unbound in CIOS |
|---|---|
| `accent-hover`, `accent-pressed`, `accent-soft` | interaction states come from Tailwind `hover:` + opacity utilities |
| `state-hover`, `state-active`, `state-selected` | same — no overlay-composite layer in the shadcn binding |
| `line-subtle`, `line-strong` | console uses a single `--border` weight |
| `shadow-1/2/3` | console elevation = surface step + border, not shadow depth |
| `on-media*`, `band-*` | **fixed family** — media/band surfaces are Stage-only. Must never enter console chrome (§3.3 rule 1) |

### 3.3 Hard rules (non-negotiable)

1. **The two color families never mix in one pair.** Theme-following (`surface-*`, `ink-*`, `line-*`, `brand-*`, `accent-*`, `state-*`) vs fixed (`on-media*`, `band-*`). A foreground and the background it sits on must come from the same family; mixing produces a pair that is correct in one theme and broken in the other. The console binds *only* theme-following tokens.
2. **`.light` / `.dark` text-color utilities are forbidden.** A type-color utility is a free-floating promise that some ancestor is dark; nothing enforces it. A band owns its background *and* its type in one rule, or it owns neither.
3. **X principle:** at most one X Blue moment per view region. In the console that moment is the active nav item (`text-accent-text` + 2px `border-accent` left rule).
4. **Focus rings are never removed or weakened.** 2px accent ring (`--ring`), 2px offset, on every interactive element.
5. **No hex literals or raw palette classes in page code.** A color with no row in §3.1 does not exist in the console.
6. **`line-input` is mandatory** on every `input`/`select`/`textarea`/checkbox outline (WCAG 1.4.11). Decorative line tokens do not meet 3:1 and must not bound controls.

### 3.4 Contrast contract

| Pair class | Minimum |
|---|---|
| any ink × surface-1/2/3 | 4.5:1 |
| accent-ink × accent / accent-hover | 4.5:1 |
| accent-text × canvas | 4.5:1 |
| status × surface-1 | 4.5:1 |
| line-input × canvas | 3.0:1 |
| accent fill × canvas | 3.0:1 |

System floor **4.70:1**, zero failing pairs across both themes. The contract is machine-checked against the canonical hex values by the PRMT-239 §5 gate-9 script (28 pairs, both themes).

### 3.5 Thermal / metric (diagrams)

`temp-supply #7DD3FC` · `temp-return #FB923C` · `metric-power` = ink-primary · `metric-heat` = critical (per theme) · `metric-flow #22D3EE`. Diagram-only; never in chrome. Thermal identity must survive a theme switch, so thermal series are fixed while `metric-power` follows the theme.

---

## 4. Typography

One family system on all surfaces: **Inter** (sans) + **ui-monospace stack** (`'SF Mono', Menlo, Consolas`). No third typeface, no serif. Unchanged in v2.0.

### 4.1 Console ramp (five steps)

Title `text-3xl/700` (page title only) · Section `text-xl/600` (rare) · Card head `font-semibold ~15.5px` (workhorse) · Body `text-sm 13.5px` (default) · Caption `text-xs 11.5px` (table headers uppercase + tracking, meta, pills).

Stage ramp (hero / section title / eyebrow / lead / stat value): see `../MDC/docs/MDCX-DESIGN-LANGUAGE.md` §4.1 — Stage surfaces are not bound in this repo.

### 4.2 Mono rules

Paths/cpaths `font-mono text-sm` · ids/slugs/scopes `font-mono text-xs` · timestamps/versions `font-mono text-xs` muted · numeric table columns `tabular-nums text-right`. Prose never mono; identifiers never sans.

---

## 5. Layout & geometry

- Spacing scale: 4 / 8 / 12 / 16 / 24 / 40 px; never off-scale values.
- Containers: content 1120px · wide 1600px · prose 68–76ch.
- Radius: **Console 6px** outer / 4px inner (buttons, inputs) per D2; status pills 999px are the only rounded exception.
- Borders do structure; shadows are near-zero. Elevation = surface step + border, not shadow depth.

## 6. Components

Console implementations live in `@cios/ui`.

- **Buttons.** Variants: primary (mono-invert), outline, ghost, destructive. Heights 34px / 30px (sm). Focus: 2px accent ring, 2px offset, never removed. One primary per view region.
- **Cards.** surface-3 (`--card`) + line border + 6px radius; padding 20px (`p-5`).
- **Alerts.** Four intents from the status quartet: `border-{intent}/45 + bg-{intent}/10 + text-{intent}`, icon slot, `role="alert|status"`. Placement under the card/section heading. Lead with outcome, then upstream detail verbatim.
- **Status pills.** Uppercase 11px/600, dot + label, 999px. Backend-state mapping (ready→ok, pending/queued/running→warn, unavailable/unknown→neutral, error→critical) is normative on both surfaces.
- **Tables.** Header `bg-muted/40`, uppercase caption type; row hover `muted/30`; right-aligned tabular numerics; empty states say *why + what to do*; row actions ghost/sm right-aligned.
- **Forms.** Label-above-input, grammar in placeholder (`sgp01`, `og_…`), mono for identifier inputs, `pattern=` mirrors of server grammar, vocabulary fields fed from API (never hardcoded arrays). Control outlines use `--input` (line-input).
- **Shell.** OpsShell / AdminShell / CustomerShell sidebar; the active item is marked with `text-accent-text` + a 2px `border-accent` left rule — the console's one accent moment.

## 7. Motion

- Console: 150ms / ease-out; async lifecycle rendered as pill + verb-specific label ("Saving…", "Linting…", never "…").
- No parallax, no auto-playing motion loops, no skeleton shimmer > 1.2s. `prefers-reduced-motion`: all non-essential transitions off (console coverage tracked as T56).

## 8. Iconography & data-viz

Stroke icons only (lucide or equivalent), 1.5px stroke, 16/20px grid, ink colors — no filled, duotone, or emoji icons in product UI. Charts: mono greys for context series, X Blue for *the* series, status colors for threshold breaches; grid lines `--border`; axis labels caption type; thermal palette per §3.5.

## 9. Voice & tone

- Numbers first: lead with the measured fact. "≤6 months to revenue" not "fast time to market".
- Publish what ships: unreleased capability is labeled `Roadmap`. No aspirational present tense.
- Sentence case everywhere except eyebrows/table headers (uppercase caption style) and product names (DC45, CIOS, MDCX).
- English on all surfaces. No exclamation marks. No superlatives without a number attached.
- Error copy: outcome first, upstream detail verbatim, then the action ("Load failed: 502 upstream — retry or check gateway logs").

## 10. Accessibility

WCAG AA: 4.5:1 body text, 3:1 large text/UI components, both themes — token discipline makes this checkable once, centrally (§3.4). Focus visible on every interactive element (2px accent ring). Full keyboard reach for all CRUD; canvas operations mirrored in tables. `role="alert|status"` on feedback; `aria-current` on active nav. Do not tint below the specified percentages.

---

## 11. Binding governance

- **Value authority:** `../MDC/design-system/tokens.css`. This file transcribes; it does not decide.
- **Role authority:** `../MDC/docs/MDCX-DESIGN-LANGUAGE.md` §3 + `../MDC/docs/THEME.md`.
- **Binding authority (which shadcn variable carries which role):** this file, §3.1.
- No new `--fog-*` keys; no parallel token systems in CIOS; no `--mdc-*` variables in the console.
- Changing a bound value = a new PRMT that updates `globals.css` and §3.1 in the same change. The two must never disagree.
- Conflicts between this document and upstream → upstream wins → escalate to Yuri.

**Deferred (T56):** responsive breakpoint tokens (10 media queries across 6 ad-hoc thresholds, no token) and `prefers-reduced-motion` coverage. Blocked on MDC defining breakpoint tokens upstream first.

---

*MDCX Design Language v2.0 (CIOS console binding) · 2026-08-31 · PRMT-239 · Sources: ../MDC/design-system/tokens.css, ../MDC/docs/MDCX-DESIGN-LANGUAGE.md (v2.0), ../MDC/docs/THEME.md.*
