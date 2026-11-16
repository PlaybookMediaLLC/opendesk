import { describe, expect, it } from "vitest";
import type { Canvas, CanvasEdge, CanvasNode } from "@/lib/api/types";
import { mergeCanvas } from "./merge";

const text = (id: string, x = 0, html = `<p>${id}</p>`): CanvasNode => ({ id, type: "text", html, x, y: 0, width: 100, height: 50 });
const edge = (id: string, fromNode: string, toNode: string, label?: string): CanvasEdge => ({ id, fromNode, toNode, ...(label ? { label } : {}) });
const doc = (nodes: CanvasNode[], edges: CanvasEdge[] = []): Canvas => ({ nodes, edges });
const ids = (c: Canvas) => c.nodes.map((n) => n.id);

describe("mergeCanvas", () => {
  const base = doc([text("a"), text("b")], [edge("e1", "a", "b")]);

  it("keeps a card someone else added while this tab moved another", () => {
    const theirs = doc([text("a"), text("b"), text("c")], [edge("e1", "a", "b")]);
    const mine = doc([text("a", 200), text("b")], [edge("e1", "a", "b")]);
    const m = mergeCanvas(base, mine, theirs);
    expect(m.conflicts).toEqual([]);
    expect(ids(m.doc)).toEqual(["a", "b", "c"]);
    expect(m.doc.nodes[0].x).toBe(200);
  });

  it("adds this tab's new cards after theirs, and takes their removals of cards it left alone", () => {
    const theirs = doc([text("a")]);
    const mine = doc([text("a"), text("b"), text("d")], [edge("e1", "a", "b"), edge("e2", "a", "d")]);
    const m = mergeCanvas(base, mine, theirs);
    expect(m.conflicts).toEqual([]);
    expect(ids(m.doc)).toEqual(["a", "d"]);
    // e1 went with their removal of b; e2 is this tab's own.
    expect(m.doc.edges.map((e) => e.id)).toEqual(["e2"]);
  });

  it("takes this tab's removal of a card they left alone", () => {
    const theirs = doc([text("a"), text("b"), text("c")], [edge("e1", "a", "b")]);
    const mine = doc([text("a")]);
    const m = mergeCanvas(base, mine, theirs);
    expect(m.conflicts).toEqual([]);
    expect(ids(m.doc)).toEqual(["a", "c"]);
    expect(m.doc.edges).toEqual([]);
  });

  it("counts a card both changed differently as a conflict and keeps this tab's", () => {
    const theirs = doc([text("a", 0, "<p>theirs</p>"), text("b")], [edge("e1", "a", "b")]);
    const mine = doc([text("a", 0, "<p>mine</p>"), text("b")], [edge("e1", "a", "b")]);
    const m = mergeCanvas(base, mine, theirs);
    expect(m.conflicts).toEqual(["a"]);
    expect(m.doc.nodes[0]).toMatchObject({ id: "a", html: "<p>mine</p>" });
  });

  it("is no conflict when both made the same change", () => {
    const same = doc([text("a", 40), text("b")], [edge("e1", "a", "b", "next")]);
    const m = mergeCanvas(base, same, structuredClone(same));
    expect(m.conflicts).toEqual([]);
    expect(m.doc).toEqual(same);
  });

  it("counts a card this tab changed that they removed, and keeps it", () => {
    const theirs = doc([text("a")]);
    const mine = doc([text("a"), text("b", 300)], [edge("e1", "a", "b")]);
    const m = mergeCanvas(base, mine, theirs);
    expect(m.conflicts).toEqual(["b"]);
    expect(ids(m.doc)).toEqual(["a", "b"]);
  });

  it("counts a card they changed that this tab removed, and leaves it out", () => {
    const theirs = doc([text("a"), text("b", 300)], [edge("e1", "a", "b")]);
    const mine = doc([text("a")]);
    const m = mergeCanvas(base, mine, theirs);
    expect(m.conflicts).toEqual(["b"]);
    expect(ids(m.doc)).toEqual(["a"]);
    expect(m.doc.edges).toEqual([]);
  });

  it("compares cards as the API keeps them, whatever the order of their keys", () => {
    const reordered = { height: 50, width: 100, y: 0, x: 0, html: "<p>a</p>", type: "text", id: "a" } as CanvasNode;
    const mine = doc([reordered, text("b")], [edge("e1", "a", "b")]);
    const theirs = doc([text("a"), text("b", 80)], [edge("e1", "a", "b")]);
    const m = mergeCanvas(base, mine, theirs);
    expect(m.conflicts).toEqual([]);
    expect(m.doc.nodes[1].x).toBe(80);
  });
});
