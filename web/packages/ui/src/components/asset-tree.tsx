/**
 * Presentational asset tree (Phase A list form; no 3D — see PRMT-152).
 *
 * - Builds a parent→children index from `Asset.parent`/`crn`.
 * - If `parent` is absent on a non-root node, falls back to trimming the last
 *   `.`-segment of `crn` (deterministic; spec-001 §2 cascade path).
 * - Roots are nodes with no in-tree parent.
 * - No data fetching, no router import. Renders `data-tree-node` + `data-crn`
 *   per node, with `aria-current="true"` on the focused one.
 *
 * `Asset` shape mirrors `@cios/api-client`'s `Asset` (spec-001 §2). Defined
 * locally so this package has zero runtime deps on other workspace packages
 * (UI package boundary). Route callers cast their `Asset[]` to this local type.
 */

import type { JSX } from "react";

interface Asset {
  crn: string;
  type: string;
  name?: string;
  parent?: string;
  /** Optional product model (e.g. DC45 / AC45) when projected from CMDB. */
  model?: string;
}

export interface AssetTreeProps {
  assets: Asset[];
  /** crn currently focused (highlighted with aria-current). */
  focus?: string;
  onSelect?: (crn: string) => void;
}

interface TreeNode {
  asset: Asset;
  depth: number;
  children: TreeNode[];
}

function deriveParentCrn(crn: string): string {
  const idx = crn.lastIndexOf(".");
  return idx <= 0 ? "" : crn.slice(0, idx);
}

function buildForest(assets: Asset[]): TreeNode[] {
  const byCrn = new Map<string, Asset>();
  for (const a of assets) byCrn.set(a.crn, a);

  const childMap = new Map<string, Asset[]>();
  const roots: Asset[] = [];
  for (const a of assets) {
    const explicit = a.parent;
    const parent = explicit !== undefined && explicit !== ""
      ? explicit
      : deriveParentCrn(a.crn);
    // Only treat as child if the parent is in the asset set; otherwise root.
    if (parent && byCrn.has(parent)) {
      const bucket = childMap.get(parent) ?? [];
      bucket.push(a);
      childMap.set(parent, bucket);
    } else {
      roots.push(a);
    }
  }

  // Preserve incoming order for stable rendering.
  const build = (asset: Asset, depth: number): TreeNode => ({
    asset,
    depth,
    children: (childMap.get(asset.crn) ?? []).map((c) => build(c, depth + 1)),
  });

  return roots.map((r) => build(r, 0));
}

export function AssetTree(props: AssetTreeProps): JSX.Element {
  const { assets, focus, onSelect } = props;
  const forest = buildForest(assets);

  const renderNode = (node: TreeNode): JSX.Element => (
    <li key={node.asset.crn}>
      <button
        type="button"
        data-tree-node
        data-crn={node.asset.crn}
        aria-current={focus === node.asset.crn ? "true" : undefined}
        onClick={() => onSelect?.(node.asset.crn)}
        className="block w-full text-left text-sm hover:bg-accent hover:text-accent-foreground rounded px-2 py-1"
        style={{ paddingLeft: `${node.depth * 16 + 8}px` }}
      >
        <span className="font-mono text-xs text-muted-foreground mr-2">
          {node.asset.type}
        </span>
        <span>{node.asset.name ?? node.asset.crn}</span>
        {node.asset.model ? (
          <span
            className="ml-2 rounded bg-primary/10 px-1 font-mono text-[10px] font-semibold text-primary"
            data-tree-model={node.asset.model}
          >
            {node.asset.model}
          </span>
        ) : null}
      </button>
      {node.children.length > 0 ? (
        <ul>{node.children.map(renderNode)}</ul>
      ) : null}
    </li>
  );

  return (
    <nav
      aria-label="Asset tree"
      className="overflow-auto rounded-md border bg-card p-2"
    >
      <ul className="space-y-0.5">
        {forest.map(renderNode)}
      </ul>
    </nav>
  );
}