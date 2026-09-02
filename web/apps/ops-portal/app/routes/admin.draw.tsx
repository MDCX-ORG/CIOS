/**
 * /admin/draw — Site-Draw Web v0 (L109 P821–P825).
 * 2D place + connect; layout JSON; CMDB writeback via :writeback.
 */
import {
  Form,
  useActionData,
  useLoaderData,
  useNavigation,
  useSearchParams,
} from "react-router";
import { useEffect, useMemo, useRef, useState } from "react";

import type { Route } from "./+types/admin.draw";
import { AdminShell } from "~/components/admin-shell";
import { requireAdminSession } from "~/lib/auth.server";
import { loadApi, postApi, putApi } from "~/lib/fetch";
import { ApiError } from "@cios/api-client";

type Instance = {
  id: string;
  path: string;
  type: string;
  model?: string;
  pack_type?: string;
  x: number;
  y: number;
  rot: number;
};
type Edge = {
  id: string;
  from_id: string;
  to_id: string;
  relation: string;
};
type Layout = {
  site: string;
  instances: Instance[];
  edges: Edge[];
  updated_at?: string;
  last_writeback?: {
    assets_created?: number;
    assets_updated?: number;
    edges_kept?: number;
    errors?: string[];
  };
};
type SceneJob = {
  site?: string;
  job_id?: string;
  status?: string;
  message?: string;
  out_dir?: string;
  exit_code?: number;
};
type Pack = { type: string; model: string };
type SiteOrg = { site: string; org_id: string };

export async function loader({ request }: Route.LoaderArgs) {
  const s = await requireAdminSession(request);
  const url = new URL(request.url);
  const site = (url.searchParams.get("site") ?? "sgp01").trim() || "sgp01";

  let layout: Layout = { site, instances: [], edges: [] };
  let packs: Pack[] = [];
  let packsError: string | null = null;
  let sites: string[] = [];
  /** Protocol relation verbs from GET /api/site-layouts (never hardcode UI list). */
  let relations: string[] = [];
  let sceneJob: SceneJob | null = null;
  let loadError: string | null = null;

  try {
    const so = await loadApi<{ items: SiteOrg[] }>("/api/site-orgs", s);
    sites = (so.items ?? []).map((x) => x.site);
    if (!sites.includes(site) && sites.length) {
      // keep requested site even if not registered
    }
  } catch {
    /* optional */
  }
  // Layout list carries protocol relation vocab (PRMT-223).
  try {
    const root = await loadApi<{ sites?: string[]; relations?: string[] }>(
      "/api/site-layouts",
      s,
    );
    if (Array.isArray(root.sites) && root.sites.length) {
      for (const si of root.sites) {
        if (!sites.includes(si)) sites.push(si);
      }
    }
    if (Array.isArray(root.relations) && root.relations.length) {
      relations = root.relations.map((r) => r.toLowerCase());
    }
  } catch {
    /* optional — fall through */
  }
  try {
    const mp = await loadApi<{ items: Pack[] }>("/api/model-packs", s);
    packs = mp.items ?? [];
  } catch (e) {
    // PRMT-229: no fabricated pack catalog — surface the failure instead.
    packs = [];
    packsError = e instanceof Error ? e.message : String(e);
  }
  try {
    layout = await loadApi<Layout>(
      `/api/site-layouts/${encodeURIComponent(site)}`,
      s,
    );
  } catch (e) {
    loadError = e instanceof Error ? e.message : String(e);
  }
  try {
    sceneJob = await loadApi<SceneJob>(
      `/api/site-layouts/${encodeURIComponent(site)}/scene-job`,
      s,
    );
  } catch {
    sceneJob = null;
  }

  return {
    user: s.user,
    site,
    layout,
    packs,
    sites,
    relations,
    sceneJob,
    loadError,
    packsError,
  };
}

export async function action({ request }: Route.ActionArgs) {
  const s = await requireAdminSession(request);
  const fd = await request.formData();
  const intent = String(fd.get("intent") ?? "save");
  const site = String(fd.get("site") ?? "sgp01").trim();
  const layoutRaw = String(fd.get("layout_json") ?? "");

  let layout: Layout;
  try {
    layout = JSON.parse(layoutRaw) as Layout;
    layout.site = site;
  } catch {
    return { ok: false as const, error: "invalid layout_json", result: null };
  }

  try {
    if (intent === "rebuild-scene") {
      const result = await postApi(
        `/api/site-layouts/${encodeURIComponent(site)}:rebuild-scene`,
        s,
        {},
      );
      return { ok: true as const, error: null, result, intent: "rebuild-scene" };
    }
    if (intent === "writeback") {
      const rebuild =
        String(fd.get("rebuild_scene") ?? "") === "1" ||
        String(fd.get("rebuild_scene") ?? "") === "on";
      const q = rebuild ? "?rebuild_scene=1" : "";
      const result = await postApi(
        `/api/site-layouts/${encodeURIComponent(site)}:writeback${q}`,
        s,
        layout,
      );
      return { ok: true as const, error: null, result, intent: "writeback" };
    }
    const saved = await putApi(
      `/api/site-layouts/${encodeURIComponent(site)}`,
      s,
      layout,
    );
    return { ok: true as const, error: null, result: saved, intent: "save" };
  } catch (e) {
    return {
      ok: false as const,
      error:
        e instanceof ApiError
          ? `${e.status}: ${e.message}`
          : e instanceof Error
            ? e.message
            : String(e),
      result: null,
      intent,
    };
  }
}

/** Parent prefix for types that cannot hang under site alone (types.yaml). */
function pathPrefix(site: string, type: string, instances: Instance[]): string {
  // cdu/rack/… live under pod (or tank); prefer first pod instance.
  const underPod = new Set([
    "cdu",
    "rack",
    "busbar",
    "tou",
    "pdu",
    "node",
    "gpu",
  ]);
  if (underPod.has(type)) {
    const pod = instances.find((i) => i.type === "pod");
    if (pod) return pod.path;
    // No pod yet: mint under pod000 (operator should place pod first).
    return `${site}.pod000`;
  }
  return site;
}

function nextPath(site: string, type: string, instances: Instance[]) {
  const used = new Set(instances.map((i) => i.path));
  const prefix = pathPrefix(site, type, instances);
  for (let i = 0; i < 1000; i++) {
    const p = `${prefix}.${type}${String(i).padStart(3, "0")}`;
    if (!used.has(p)) return p;
  }
  return `${prefix}.${type}999`;
}

export default function AdminDraw() {
  const { user, site, layout, packs, sites, relations, sceneJob, loadError, packsError } =
    useLoaderData<typeof loader>();
  const actionData = useActionData<typeof action>();
  const nav = useNavigation();
  const busy = nav.state !== "idle";
  const [, setSearch] = useSearchParams();

  const relationOptions = useMemo(() => {
    if (relations.length > 0) return relations;
    // Loader empty only if API down; keep empty so UI does not invent verbs.
    return [] as string[];
  }, [relations]);

  const [draft, setDraft] = useState<Layout>(() => ({
    site: layout.site || site,
    instances: layout.instances ?? [],
    edges: layout.edges ?? [],
  }));
  // H4: re-seed draft when loader site/layout changes (site select must not
  // keep the previous site's instances under a new site slug).
  useEffect(() => {
    setDraft({
      site: layout.site || site,
      instances: layout.instances ?? [],
      edges: layout.edges ?? [],
    });
    setSelected(null);
    setSelectedEdge(null);
    setConnectFrom(null);
    setEdgeHint(null);
    // Depend on site + loader revision only — not draft mutations.
  }, [site, layout.site, layout.updated_at]);
  const [mode, setMode] = useState<"place" | "connect" | "select">("select");
  const [placeType, setPlaceType] = useState("pod");
  const [placeModel, setPlaceModel] = useState(packs[0]?.model ?? "");
  const [selected, setSelected] = useState<string | null>(null);
  const [selectedEdge, setSelectedEdge] = useState<string | null>(null);
  const [connectFrom, setConnectFrom] = useState<string | null>(null);
  const [relation, setRelation] = useState(() => relations[0] ?? "");
  // Pick first protocol relation once vocab arrives (PRMT-223).
  useEffect(() => {
    if (relationOptions.length && !relationOptions.includes(relation)) {
      setRelation(relationOptions[0]!);
    }
  }, [relationOptions, relation]);
  const [edgeHint, setEdgeHint] = useState<string | null>(null);
  const [pointerSvg, setPointerSvg] = useState<{ x: number; y: number } | null>(
    null,
  );
  const [dragId, setDragId] = useState<string | null>(null);
  const dragMoved = useRef(false);
  const edgeRowRefs = useRef<Record<string, HTMLTableRowElement | null>>({});

  // Mutual exclusion: select node clears edge and vice versa.
  function selectNode(id: string | null) {
    setSelected(id);
    if (id != null) setSelectedEdge(null);
  }
  function selectEdge(id: string | null) {
    setSelectedEdge(id);
    if (id != null) {
      setSelected(null);
      setMode("select");
    }
  }

  const packTypes = useMemo(() => {
    const t = new Set(packs.map((p) => p.type));
    // Protocol leaf types commonly used in site-draw (types.yaml).
    ["pod", "cdu", "chiller", "ups"].forEach((x) => t.add(x));
    return [...t];
  }, [packs]);

  const modelsForType = packs.filter((p) => p.type === placeType);

  function svgPoint(
    e: React.MouseEvent<SVGSVGElement> | React.MouseEvent,
    svg: SVGSVGElement,
  ) {
    const rect = svg.getBoundingClientRect();
    return {
      x: ((e.clientX - rect.left) / rect.width) * 800,
      y: ((e.clientY - rect.top) / rect.height) * 480,
    };
  }

  function onCanvasClick(e: React.MouseEvent<SVGSVGElement>) {
    if (dragMoved.current) {
      dragMoved.current = false;
      return;
    }
    const { x, y } = svgPoint(e, e.currentTarget);
    if (mode === "place") {
      const id = `i_${Date.now().toString(36)}`;
      const path = nextPath(draft.site, placeType, draft.instances);
      setDraft((d) => ({
        ...d,
        instances: [
          ...d.instances,
          {
            id,
            path,
            type: placeType,
            model: placeModel,
            pack_type: placeType,
            x,
            y,
            rot: 0,
          },
        ],
      }));
      selectNode(id);
      return;
    }
    if (mode === "select") {
      selectNode(null);
      selectEdge(null);
    }
  }

  function onCanvasMove(e: React.MouseEvent<SVGSVGElement>) {
    const pt = svgPoint(e, e.currentTarget);
    setPointerSvg(pt);
    if (dragId) {
      dragMoved.current = true;
      setDraft((d) => ({
        ...d,
        instances: d.instances.map((i) =>
          i.id === dragId ? { ...i, x: pt.x, y: pt.y } : i,
        ),
      }));
    }
  }

  function onInstancePointerDown(id: string, e: React.MouseEvent) {
    e.stopPropagation();
    if (mode === "connect") {
      onInstanceClick(id, e);
      return;
    }
    selectNode(id);
    setDragId(id);
    dragMoved.current = false;
  }

  function onInstanceClick(id: string, e: React.MouseEvent) {
    e.stopPropagation();
    if (mode === "connect") {
      if (!connectFrom) {
        setConnectFrom(id);
        setEdgeHint(null);
        return;
      }
      if (connectFrom === id) {
        setConnectFrom(null);
        return;
      }
      // Avoid duplicate edge same pair+relation.
      const dup = draft.edges.some(
        (ed) =>
          ed.from_id === connectFrom &&
          ed.to_id === id &&
          ed.relation === relation,
      );
      if (dup) {
        setEdgeHint(
          "That connection already exists with this relation. Choose another target or relation.",
        );
        setConnectFrom(null);
        return;
      }
      const eid = `e_${Date.now().toString(36)}`;
      setDraft((d) => ({
        ...d,
        edges: [
          ...d.edges,
          {
            id: eid,
            from_id: connectFrom,
            to_id: id,
            relation,
          },
        ],
      }));
      setConnectFrom(null);
      setEdgeHint(null);
      selectEdge(eid);
      return;
    }
    selectNode(id);
  }

  function moveSelected(dx: number, dy: number) {
    if (!selected) return;
    setDraft((d) => ({
      ...d,
      instances: d.instances.map((i) =>
        i.id === selected ? { ...i, x: i.x + dx, y: i.y + dy } : i,
      ),
    }));
  }

  function deleteSelected() {
    if (!selected) return;
    const inst = draft.instances.find((i) => i.id === selected);
    const label = inst?.path || selected;
    if (
      !window.confirm(
        `Delete instance ${label} and its edges from the draft?`,
      )
    ) {
      return;
    }
    setDraft((d) => ({
      ...d,
      instances: d.instances.filter((i) => i.id !== selected),
      edges: d.edges.filter(
        (e) => e.from_id !== selected && e.to_id !== selected,
      ),
    }));
    selectNode(null);
  }

  function deleteEdge(eid: string) {
    setDraft((d) => ({
      ...d,
      edges: d.edges.filter((e) => e.id !== eid),
    }));
    if (selectedEdge === eid) selectEdge(null);
    setEdgeHint(null);
  }

  function edgeConflict(
    from: string,
    to: string,
    rel: string,
    exceptId?: string,
  ): boolean {
    return draft.edges.some(
      (ed) =>
        ed.id !== exceptId &&
        ed.from_id === from &&
        ed.to_id === to &&
        ed.relation === rel,
    );
  }

  function changeEdgeRelation(eid: string, rel: string) {
    const edge = draft.edges.find((e) => e.id === eid);
    if (!edge || edge.relation === rel) return;
    if (edgeConflict(edge.from_id, edge.to_id, rel, eid)) {
      setEdgeHint(
        `Cannot set relation to “${rel}”: an identical edge already exists.`,
      );
      return;
    }
    setDraft((d) => ({
      ...d,
      edges: d.edges.map((e) =>
        e.id === eid ? { ...e, relation: rel } : e,
      ),
    }));
    setEdgeHint(null);
  }

  function reverseEdge(eid: string) {
    const edge = draft.edges.find((e) => e.id === eid);
    if (!edge) return;
    const nf = edge.to_id;
    const nt = edge.from_id;
    if (edgeConflict(nf, nt, edge.relation, eid)) {
      setEdgeHint(
        "Cannot reverse: the opposite direction with this relation already exists.",
      );
      return;
    }
    setDraft((d) => ({
      ...d,
      edges: d.edges.map((e) =>
        e.id === eid ? { ...e, from_id: nf, to_id: nt } : e,
      ),
    }));
    setEdgeHint(null);
  }

  // Scroll edge table row into view when canvas selects an edge.
  useEffect(() => {
    if (!selectedEdge) return;
    const row = edgeRowRefs.current[selectedEdge];
    row?.scrollIntoView({ block: "nearest", behavior: "smooth" });
  }, [selectedEdge]);

  // Keyboard: Esc cancel connect/selection, Delete remove edge or node, arrows nudge.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      const tag = (e.target as HTMLElement | null)?.tagName;
      if (tag === "INPUT" || tag === "SELECT" || tag === "TEXTAREA") return;
      if (e.key === "Escape") {
        setConnectFrom(null);
        setDragId(null);
        selectEdge(null);
        selectNode(null);
        setEdgeHint(null);
        if (mode === "connect") setMode("select");
        return;
      }
      if (e.key === "Delete" || e.key === "Backspace") {
        if (selectedEdge) {
          e.preventDefault();
          deleteEdge(selectedEdge);
          return;
        }
        if (selected) {
          e.preventDefault();
          deleteSelected();
        }
        return;
      }
      if (!selected) return;
      const step = e.shiftKey ? 20 : 10;
      if (e.key === "ArrowLeft") {
        e.preventDefault();
        moveSelected(-step, 0);
      } else if (e.key === "ArrowRight") {
        e.preventDefault();
        moveSelected(step, 0);
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        moveSelected(0, -step);
      } else if (e.key === "ArrowDown") {
        e.preventDefault();
        moveSelected(0, step);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [selected, selectedEdge, mode, draft.instances, draft.edges]);

  useEffect(() => {
    function up() {
      setDragId(null);
    }
    window.addEventListener("mouseup", up);
    return () => window.removeEventListener("mouseup", up);
  }, []);

  const byId = Object.fromEntries(draft.instances.map((i) => [i.id, i]));
  const connectFromInst = connectFrom ? byId[connectFrom] : null;
  const selectedEdgeObj = selectedEdge
    ? draft.edges.find((e) => e.id === selectedEdge) ?? null
    : null;

  return (
    <AdminShell title="Site draw" active="draw">
      <section
        key={site}
        className="rounded-md border bg-card p-5"
        data-admin-draw
      >
        <div className="flex flex-wrap items-end justify-between gap-3">
          <div>
            <h2 className="font-semibold">Site layout (2D)</h2>
            <p className="mt-1 text-sm text-muted-foreground">
              Select to drag · Place to drop gear · Connect for edges. Arrow
              keys nudge · Delete removes · Esc cancels connect. admin=
              {user.sub}
            </p>
          </div>
          <label className="text-sm">
            <span className="text-muted-foreground">Site</span>
            <select
              className="ml-2 rounded border bg-background px-2 py-1 font-mono text-sm"
              value={draft.site}
              onChange={(e) => {
                const v = e.target.value;
                setSearch({ site: v });
                setDraft((d) => ({ ...d, site: v }));
              }}
              data-admin-draw-site
            >
              {[draft.site, ...sites].filter((v, i, a) => a.indexOf(v) === i).map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </label>
        </div>

        {loadError ? (
          <p className="mt-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-sm text-warning">
            Load warning: {loadError} (starting empty layout)
          </p>
        ) : null}
        {packsError ? (
          <p
            className="mt-2 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            role="alert"
            data-draw-packs-error
          >
            Model-pack catalog unavailable ({packsError}) — the Place palette
            stays empty until /api/model-packs recovers.
          </p>
        ) : null}
        {actionData?.ok === false ? (
          <p className="mt-2 rounded border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            {actionData.error}
          </p>
        ) : null}
        {actionData?.ok === true ? (
          <p className="mt-2 rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success">
            {actionData.intent === "writeback"
              ? "Writeback done."
              : actionData.intent === "rebuild-scene"
                ? "Scene rebuild kicked."
                : "Layout saved."}
          </p>
        ) : null}

        <div className="mt-4 flex flex-wrap items-center gap-2 text-sm">
          <div
            className="inline-flex rounded-md border border-border p-0.5"
            role="group"
            aria-label="Draw mode"
            data-admin-draw-mode
          >
            {(
              [
                ["select", "Select / drag"],
                ["place", "Place"],
                ["connect", "Connect"],
              ] as const
            ).map(([m, label]) => (
              <button
                key={m}
                type="button"
                className={
                  mode === m
                    ? "rounded border-l-2 border-accent px-2.5 py-1 font-medium text-accent-text"
                    : "rounded border-l-2 border-transparent px-2.5 py-1 text-muted-foreground hover:text-foreground"
                }
                onClick={() => {
                  setMode(m);
                  setConnectFrom(null);
                }}
                data-admin-draw-mode-btn={m}
              >
                {label}
              </button>
            ))}
          </div>
          {mode === "place" ? (
            <>
              <label>
                Type{" "}
                <select
                  value={placeType}
                  onChange={(e) => {
                    setPlaceType(e.target.value);
                    const m = packs.find((p) => p.type === e.target.value);
                    if (m) setPlaceModel(m.model);
                  }}
                  className="rounded border bg-background px-2 py-1 font-mono"
                >
                  {packTypes.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                Model{" "}
                <select
                  value={placeModel}
                  onChange={(e) => setPlaceModel(e.target.value)}
                  className="rounded border bg-background px-2 py-1 font-mono"
                >
                  {(modelsForType.length
                    ? modelsForType
                    : [{ model: placeModel }]
                  ).map((p) => (
                    <option key={p.model} value={p.model}>
                      {p.model}
                    </option>
                  ))}
                </select>
              </label>
              <span className="text-xs text-muted-foreground">
                Click empty canvas to place
              </span>
            </>
          ) : null}
          {mode === "connect" ? (
            <label className="flex flex-wrap items-center gap-2">
              Relation{" "}
              <select
                value={relation}
                onChange={(e) => setRelation(e.target.value)}
                className="rounded border bg-background px-2 py-1 font-mono"
                data-admin-draw-relation
              >
                {relationOptions.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
              {relationOptions.length === 0 ? (
                <span className="text-xs text-warning">
                  Relation vocab unavailable (API)
                </span>
              ) : connectFromInst ? (
                <span className="font-mono text-xs text-warning">
                  from {connectFromInst.path} — click target (Esc cancel)
                </span>
              ) : (
                <span className="text-xs text-muted-foreground">
                  click source instance
                </span>
              )}
            </label>
          ) : null}
          {mode === "select" ? (
            <span className="text-xs text-muted-foreground">
              Drag instances · arrows nudge · Delete removes
            </span>
          ) : null}
          <button
            type="button"
            className="rounded border px-2 py-1 text-destructive disabled:opacity-40"
            onClick={deleteSelected}
            disabled={!selected}
            data-admin-draw-delete
          >
            Delete
          </button>
        </div>

        <svg
          viewBox="0 0 800 480"
          className={
            mode === "place"
              ? "mt-3 h-80 w-full cursor-crosshair rounded-md border bg-muted/20"
              : mode === "connect"
                ? "mt-3 h-80 w-full cursor-cell rounded-md border bg-muted/20"
                : "mt-3 h-80 w-full cursor-default rounded-md border bg-muted/20"
          }
          onClick={onCanvasClick}
          onMouseMove={onCanvasMove}
          onMouseLeave={() => {
            setPointerSvg(null);
            setDragId(null);
          }}
          data-admin-draw-canvas
        >
          {/* grid */}
          {Array.from({ length: 16 }).map((_, i) => (
            <line
              key={`v${i}`}
              x1={i * 50}
              y1={0}
              x2={i * 50}
              y2={480}
              stroke="currentColor"
              className="text-border"
              strokeWidth={0.5}
            />
          ))}
          {Array.from({ length: 10 }).map((_, i) => (
            <line
              key={`h${i}`}
              x1={0}
              y1={i * 48}
              x2={800}
              y2={i * 48}
              stroke="currentColor"
              className="text-border"
              strokeWidth={0.5}
            />
          ))}
          {draft.edges.map((e) => {
            const a = byId[e.from_id];
            const b = byId[e.to_id];
            if (!a || !b) return null;
            const isSel = selectedEdge === e.id;
            return (
              <g key={e.id} data-admin-draw-edge={e.id}>
                {/* Visible stroke */}
                <line
                  x1={a.x}
                  y1={a.y}
                  x2={b.x}
                  y2={b.y}
                  stroke="currentColor"
                  className={
                    isSel ? "text-accent-text" : "text-foreground/70"
                  }
                  strokeWidth={isSel ? 3 : 2}
                  markerEnd={isSel ? "url(#arrow-accent)" : "url(#arrow)"}
                />
                {/* Wide transparent hit target (PRMT-223) — drawn after so on top */}
                <line
                  x1={a.x}
                  y1={a.y}
                  x2={b.x}
                  y2={b.y}
                  stroke="transparent"
                  strokeWidth={14}
                  style={{ pointerEvents: "stroke", cursor: "pointer" }}
                  onClick={(ev) => {
                    ev.stopPropagation();
                    selectEdge(e.id);
                  }}
                  data-admin-draw-edge-hit={e.id}
                />
                <text
                  x={(a.x + b.x) / 2}
                  y={(a.y + b.y) / 2 - 6}
                  className={
                    isSel
                      ? "fill-accent text-[10px] font-medium"
                      : "fill-muted-foreground text-[10px]"
                  }
                  style={{ pointerEvents: "none" }}
                >
                  {e.relation}
                </text>
              </g>
            );
          })}
          {/* Rubber-band while connecting */}
          {mode === "connect" && connectFromInst && pointerSvg ? (
            <line
              x1={connectFromInst.x}
              y1={connectFromInst.y}
              x2={pointerSvg.x}
              y2={pointerSvg.y}
              stroke="currentColor"
              className="text-accent-text"
              strokeWidth={1.5}
              strokeDasharray="6 4"
              data-admin-draw-rubberband
            />
          ) : null}
          <defs>
            <marker
              id="arrow"
              markerWidth="8"
              markerHeight="8"
              refX="6"
              refY="3"
              orient="auto"
            >
              <path d="M0,0 L6,3 L0,6 Z" className="fill-foreground/70" />
            </marker>
            <marker
              id="arrow-accent"
              markerWidth="8"
              markerHeight="8"
              refX="6"
              refY="3"
              orient="auto"
            >
              <path d="M0,0 L6,3 L0,6 Z" className="fill-accent" />
            </marker>
          </defs>
          {draft.instances.map((inst) => {
            const isSel = selected === inst.id;
            const isSrc = connectFrom === inst.id;
            return (
              <g
                key={inst.id}
                transform={`translate(${inst.x},${inst.y}) rotate(${inst.rot})`}
                onMouseDown={(ev) => onInstancePointerDown(inst.id, ev)}
                onClick={(ev) => {
                  if (mode === "connect") onInstanceClick(inst.id, ev);
                }}
                className={
                  mode === "select" || mode === "place"
                    ? "cursor-grab active:cursor-grabbing"
                    : "cursor-pointer"
                }
                data-admin-draw-instance={inst.id}
              >
                <rect
                  x={-36}
                  y={-20}
                  width={72}
                  height={40}
                  rx={4}
                  className={
                    isSrc
                      ? "fill-accent/15 stroke-accent"
                      : isSel
                        ? "fill-primary/20 stroke-primary"
                        : "fill-card stroke-border"
                  }
                  strokeWidth={isSel || isSrc ? 2 : 1}
                />
                <text
                  textAnchor="middle"
                  y={-2}
                  className="fill-foreground font-mono text-[10px]"
                >
                  {inst.type}
                </text>
                <text
                  textAnchor="middle"
                  y={12}
                  className="fill-muted-foreground font-mono text-[9px]"
                >
                  {inst.model || inst.path.split(".").pop()}
                </text>
              </g>
            );
          })}
        </svg>

        {edgeHint ? (
          <p
            className="mt-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-sm text-warning"
            role="status"
            data-admin-draw-edge-hint
          >
            {edgeHint}
          </p>
        ) : null}

        {selectedEdgeObj ? (
          <div
            className="mt-3 flex flex-wrap items-center gap-3 rounded-md border border-accent/40 bg-card px-3 py-2 text-sm"
            data-admin-draw-edge-inspector
          >
            <span className="text-xs font-medium text-accent-text">Edge</span>
            <span className="font-mono text-xs">
              {byId[selectedEdgeObj.from_id]?.path ?? selectedEdgeObj.from_id}
              {" → "}
              {byId[selectedEdgeObj.to_id]?.path ?? selectedEdgeObj.to_id}
            </span>
            <label className="flex items-center gap-1 text-xs">
              Relation
              <select
                className="rounded border bg-background px-2 py-1 font-mono"
                value={selectedEdgeObj.relation}
                onChange={(e) =>
                  changeEdgeRelation(selectedEdgeObj.id, e.target.value)
                }
                data-admin-draw-edge-relation
              >
                {relationOptions.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
            </label>
            <button
              type="button"
              className="rounded border px-2 py-1 text-xs"
              onClick={() => reverseEdge(selectedEdgeObj.id)}
              data-admin-draw-edge-reverse
            >
              Reverse
            </button>
            <button
              type="button"
              className="rounded bg-destructive px-2 py-1 text-xs font-medium text-destructive-foreground"
              onClick={() => deleteEdge(selectedEdgeObj.id)}
              data-admin-draw-edge-delete-inspector
            >
              Delete edge
            </button>
          </div>
        ) : null}

        <div className="mt-3 grid gap-3 sm:grid-cols-2">
          <div className="max-h-36 overflow-auto rounded-md border text-xs font-mono">
            <table className="w-full text-left" data-admin-draw-instances>
              <thead className="bg-muted/40 text-muted-foreground">
                <tr>
                  <th className="px-2 py-1">path</th>
                  <th className="px-2 py-1">type</th>
                  <th className="px-2 py-1">xy</th>
                </tr>
              </thead>
              <tbody>
                {draft.instances.length === 0 ? (
                  <tr>
                    <td
                      colSpan={3}
                      className="px-2 py-3 text-muted-foreground"
                    >
                      No instances — switch to Place and click the canvas.
                    </td>
                  </tr>
                ) : (
                  draft.instances.map((i) => (
                    <tr
                      key={i.id}
                      className={
                        selected === i.id
                          ? "cursor-pointer border-t bg-muted/40"
                          : "cursor-pointer border-t"
                      }
                      onClick={() => {
                        selectNode(i.id);
                        setMode("select");
                      }}
                    >
                      <td className="px-2 py-1">{i.path}</td>
                      <td className="px-2 py-1">{i.type}</td>
                      <td className="px-2 py-1">
                        {Math.round(i.x)},{Math.round(i.y)}
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
          <div className="max-h-36 overflow-auto rounded-md border text-xs font-mono">
            <table className="w-full text-left" data-admin-draw-edges>
              <thead className="bg-muted/40 text-muted-foreground">
                <tr>
                  <th className="px-2 py-1">from → to</th>
                  <th className="px-2 py-1">rel</th>
                  <th className="px-2 py-1" />
                </tr>
              </thead>
              <tbody>
                {draft.edges.length === 0 ? (
                  <tr>
                    <td
                      colSpan={3}
                      className="px-2 py-3 text-muted-foreground"
                    >
                      No edges — Connect mode: source then target.
                    </td>
                  </tr>
                ) : (
                  draft.edges.map((e) => {
                    const a = byId[e.from_id];
                    const b = byId[e.to_id];
                    const isSel = selectedEdge === e.id;
                    return (
                      <tr
                        key={e.id}
                        ref={(el) => {
                          edgeRowRefs.current[e.id] = el;
                        }}
                        tabIndex={0}
                        className={
                          isSel
                            ? "cursor-pointer border-t border-l-2 border-l-accent bg-accent/10 outline-none"
                            : "cursor-pointer border-t border-l-2 border-l-transparent outline-none focus:bg-muted/30"
                        }
                        onClick={() => selectEdge(e.id)}
                        onKeyDown={(ev) => {
                          if (ev.key === "Enter" || ev.key === " ") {
                            ev.preventDefault();
                            selectEdge(e.id);
                          }
                        }}
                        data-admin-draw-edge-row={e.id}
                        aria-selected={isSel}
                      >
                        <td className="px-2 py-1">
                          {(a?.path ?? e.from_id)
                            .split(".")
                            .slice(-2)
                            .join(".")}{" "}
                          →{" "}
                          {(b?.path ?? e.to_id).split(".").slice(-2).join(".")}
                        </td>
                        <td className="px-2 py-1" onClick={(ev) => ev.stopPropagation()}>
                          <select
                            className="w-full rounded border bg-background px-1 py-0.5 font-mono text-xs"
                            value={e.relation}
                            onChange={(ev) =>
                              changeEdgeRelation(e.id, ev.target.value)
                            }
                            aria-label={`Relation for edge ${e.id}`}
                          >
                            {relationOptions.map((r) => (
                              <option key={r} value={r}>
                                {r}
                              </option>
                            ))}
                          </select>
                        </td>
                        <td className="px-2 py-1 text-right">
                          <button
                            type="button"
                            className="text-destructive hover:underline"
                            onClick={(ev) => {
                              ev.stopPropagation();
                              deleteEdge(e.id);
                            }}
                            data-admin-draw-edge-delete
                          >
                            Remove
                          </button>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
        </div>

        <Form method="post" className="mt-4 flex flex-wrap items-center gap-2">
          <input type="hidden" name="site" value={draft.site} />
          <input
            type="hidden"
            name="layout_json"
            value={JSON.stringify(draft)}
          />
          <button
            type="submit"
            name="intent"
            value="save"
            disabled={busy}
            className="rounded bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground disabled:opacity-50"
            data-admin-draw-save
          >
            Save layout
          </button>
          <button
            type="submit"
            name="intent"
            value="writeback"
            disabled={busy}
            className="rounded border border-warning/60 px-3 py-1.5 text-sm text-warning disabled:opacity-50"
            data-admin-draw-writeback
            onClick={(e) => {
              if (
                !window.confirm(
                  `Writeback layout for ${draft.site} → CMDB? Existing assets keep curated fields; layout keys are merged.`,
                )
              ) {
                e.preventDefault();
              }
            }}
          >
            Writeback → CMDB
          </button>
          <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <input
              type="checkbox"
              name="rebuild_scene"
              value="1"
              defaultChecked
              data-admin-draw-rebuild
            />
            Rebuild scene after writeback
          </label>
          <button
            type="submit"
            name="intent"
            value="rebuild-scene"
            disabled={busy}
            className="rounded border px-3 py-1.5 text-sm disabled:opacity-50"
            data-admin-draw-rebuild-only
          >
            Rebuild scene only
          </button>
        </Form>
        {layout.last_writeback ? (
          <p className="mt-2 text-xs text-muted-foreground">
            Last writeback: +{layout.last_writeback.assets_created ?? 0} created /{" "}
            {layout.last_writeback.assets_updated ?? 0} updated · edges{" "}
            {layout.last_writeback.edges_kept ?? 0}
          </p>
        ) : null}
        {(() => {
          // RR SerializeFrom can collapse heterogeneous action unions; narrow locally.
          const ad = actionData as
            | {
                ok: true;
                result: unknown;
                intent?: string;
              }
            | { ok: false; error: string; result: null; intent?: string }
            | undefined;
          let fromAction: SceneJob | null = null;
          if (ad?.ok && ad.result && typeof ad.result === "object") {
            const r = ad.result as { scene_job?: SceneJob } & SceneJob;
            fromAction =
              r.scene_job ?? (ad.intent === "rebuild-scene" ? r : null);
          }
          const job = fromAction ?? sceneJob;
          if (!job || !job.status || job.status === "none") return null;
          return (
            <p
              className="mt-2 text-xs font-mono text-muted-foreground"
              data-admin-draw-scene-job
            >
              Scene job: {job.status}
              {job.message ? ` — ${job.message}` : ""}
              {job.out_dir ? ` · ${job.out_dir}` : ""}
            </p>
          );
        })()}
      </section>
    </AdminShell>
  );
}
