// Merging a canvas this tab changed with one someone else saved since (contracts/wiki.md
// "Canvas"). Three canvases: the one this tab edited from (base), this tab's (mine) and the
// stored one (theirs). Each card and connection is taken by its id: from theirs when this tab
// left it as it was, from mine when they did, and from mine (counted as a conflict) when both
// changed it differently, so "keep mine" saves exactly what the merge shows.

import type { Canvas, CanvasEdge, CanvasNode } from "@/lib/api/types";
import { toApi } from "./model";

export type Merged = {
  doc: Canvas;
  /** Ids of the cards and connections both sides changed differently (mine kept). */
  conflicts: string[];
};

/** One spelling of a card or connection whatever its keys' order; "" for none. */
function itemKey(x: object | undefined): string {
  if (!x) return "";
  return JSON.stringify(Object.fromEntries(Object.entries(x).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))));
}

function mergeItems<T extends CanvasNode | CanvasEdge>(base: readonly T[], mine: readonly T[], theirs: readonly T[], conflicts: string[]): T[] {
  const byId = (xs: readonly T[]) => new Map(xs.map((x) => [x.id, x]));
  const B = byId(base);
  const M = byId(mine);
  const T = byId(theirs);
  const pick = (id: string): T | undefined => {
    const b = itemKey(B.get(id));
    const m = itemKey(M.get(id));
    const t = itemKey(T.get(id));
    if (m === b) return T.get(id);
    if (t === b || t === m) return M.get(id);
    conflicts.push(id);
    return M.get(id);
  };
  const out: T[] = [];
  // Their order first (the stored z order), then what only this tab has, in its order.
  for (const x of theirs) {
    const v = pick(x.id);
    if (v) out.push(v);
  }
  for (const x of mine) {
    if (T.has(x.id)) continue;
    const v = pick(x.id);
    if (v) out.push(v);
  }
  return out;
}

/** Merges this tab's canvas (mine) with theirs, both made from base. */
export function mergeCanvas(base: Canvas, mine: Canvas, theirs: Canvas): Merged {
  const b = toApi(base);
  const m = toApi(mine);
  const t = toApi(theirs);
  const conflicts: string[] = [];
  const nodes = mergeItems(b.nodes, m.nodes, t.nodes, conflicts);
  const ids = new Set(nodes.map((n) => n.id));
  // A connection goes with the cards it joins: one whose card is gone is left out, not a conflict.
  const edgeConflicts: string[] = [];
  const edges = mergeItems(b.edges, m.edges, t.edges, edgeConflicts).filter((e) => ids.has(e.fromNode) && ids.has(e.toNode));
  const kept = new Set(edges.map((e) => e.id));
  return { doc: { nodes, edges }, conflicts: [...conflicts, ...edgeConflicts.filter((id) => kept.has(id))] };
}
