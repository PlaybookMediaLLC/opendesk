import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, useReducer } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api/client";
import type { Canvas, CanvasNode, WikiPage, WikiPagePatch } from "@/lib/api/types";
import { qk } from "@/lib/queries";
import { fromApi, historyReducer, initHistory, type HistoryAction } from "./model";
import { useCanvasSync } from "./useCanvasSync";

const update = vi.fn<(id: string, b: WikiPagePatch) => Promise<WikiPage>>();
const get = vi.fn<(id: string) => Promise<WikiPage>>();
vi.mock("@/lib/api/endpoints", () => ({ api: { wiki: { update: (id: string, b: WikiPagePatch) => update(id, b), get: (id: string) => get(id) } } }));
vi.mock("@/components/ui/toast", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const text = (id: string, x = 0, html = `<p>${id}</p>`): CanvasNode => ({ id, type: "text", html, x, y: 0, width: 100, height: 60 });
const pageOf = (canvas: Canvas, canvas_version: number, by = "Ana"): WikiPage =>
  ({ id: "c1", kind: "canvas", canvas, canvas_version, version: 1, updated_at: "", updated_by: { id: "u2", name: by } }) as unknown as WikiPage;

beforeEach(() => {
  vi.useFakeTimers();
  update.mockReset();
  get.mockReset();
});
afterEach(() => {
  vi.useRealTimers();
  document.body.innerHTML = "";
});

function mount(page: WikiPage) {
  const qc = new QueryClient();
  qc.setQueryData(qk.wikiPage(page.id), page);
  const out: { doc: Canvas; dispatch: (a: HistoryAction) => void; sync: ReturnType<typeof useCanvasSync> } = {} as never;
  function Harness() {
    const live = (qc.getQueryData<WikiPage>(qk.wikiPage(page.id)) ?? page) as WikiPage;
    const [h, dispatch] = useReducer(historyReducer, live.canvas, (c) => initHistory(fromApi(c)));
    out.doc = h.present;
    out.dispatch = dispatch;
    out.sync = useCanvasSync(live, true, h.present, dispatch);
    return null;
  }
  const host = document.createElement("div");
  document.body.appendChild(host);
  const root = createRoot(host);
  const render = () => act(async () => root.render(<QueryClientProvider client={qc}><Harness /></QueryClientProvider>));
  return { qc, out, render, unmount: () => act(async () => root.unmount()) };
}

const conflict = () => new ApiError({ status: 409, code: "canvas_changed", detail: "changed" });

it("sends the canvas_version it edited from and moves to the saved one", async () => {
  const base = { nodes: [text("a")], edges: [] };
  const { out, render, unmount } = mount(pageOf(base, 3));
  await render();
  const moved = { nodes: [text("a", 40)], edges: [] };
  update.mockResolvedValueOnce(pageOf(moved, 4));
  await act(async () => out.dispatch({ type: "set", doc: moved }));
  await act(async () => vi.advanceTimersByTimeAsync(800));
  expect(update).toHaveBeenCalledTimes(1);
  expect(update.mock.calls[0][1]).toMatchObject({ canvas_version: 3 });
  const next = { nodes: [text("a", 80)], edges: [] };
  update.mockResolvedValueOnce(pageOf(next, 5));
  await act(async () => out.dispatch({ type: "set", doc: next }));
  await act(async () => vi.advanceTimersByTimeAsync(800));
  expect(update.mock.calls[1][1]).toMatchObject({ canvas_version: 4 });
  await unmount();
});

it("merges a refused save with the canvas saved since, and saves the merge with its version", async () => {
  const base = { nodes: [text("a"), text("b")], edges: [] };
  const { out, render, unmount } = mount(pageOf(base, 1));
  await render();
  // Someone else added c (version 2) while this tab moved a.
  const theirs = { nodes: [text("a"), text("b"), text("c")], edges: [] };
  update.mockRejectedValueOnce(conflict());
  get.mockResolvedValueOnce(pageOf(theirs, 2));
  update.mockImplementationOnce(async (_id, b) => pageOf(b.canvas!, 3));
  await act(async () => out.dispatch({ type: "set", doc: { nodes: [text("a", 200), text("b")], edges: [] } }));
  await act(async () => vi.advanceTimersByTimeAsync(800));
  await act(async () => vi.advanceTimersByTimeAsync(800));
  expect(get).toHaveBeenCalledTimes(1);
  expect(update).toHaveBeenCalledTimes(2);
  const second = update.mock.calls[1][1];
  expect(second.canvas_version).toBe(2);
  expect(second.canvas!.nodes.map((n) => n.id)).toEqual(["a", "b", "c"]);
  expect(second.canvas!.nodes[0].x).toBe(200);
  expect(out.sync.conflict).toBeNull();
  await unmount();
});

it("asks when both changed one card: load theirs drops this tab's change", async () => {
  const base = { nodes: [text("a")], edges: [] };
  const { out, render, unmount } = mount(pageOf(base, 1));
  await render();
  update.mockRejectedValueOnce(conflict());
  get.mockResolvedValueOnce(pageOf({ nodes: [text("a", 0, "<p>theirs</p>")], edges: [] }, 2, "Ana"));
  await act(async () => out.dispatch({ type: "set", doc: { nodes: [text("a", 0, "<p>mine</p>")], edges: [] } }));
  await act(async () => vi.advanceTimersByTimeAsync(800));
  await act(async () => vi.advanceTimersByTimeAsync(800));
  expect(out.sync.conflict).toMatchObject({ by: { name: "Ana" }, count: 1 });
  // Nothing more is saved while the person decides.
  expect(update).toHaveBeenCalledTimes(1);
  expect((out.doc.nodes[0] as { html: string }).html).toBe("<p>mine</p>");
  await act(async () => out.sync.loadTheirs());
  expect(out.sync.conflict).toBeNull();
  expect((out.doc.nodes[0] as { html: string }).html).toBe("<p>theirs</p>");
  await act(async () => vi.advanceTimersByTimeAsync(800));
  expect(update).toHaveBeenCalledTimes(1);
  await unmount();
});

it("keep mine saves this tab's side with their version", async () => {
  const base = { nodes: [text("a")], edges: [] };
  const { out, render, unmount } = mount(pageOf(base, 1));
  await render();
  update.mockRejectedValueOnce(conflict());
  get.mockResolvedValueOnce(pageOf({ nodes: [text("a", 0, "<p>theirs</p>")], edges: [] }, 2));
  await act(async () => out.dispatch({ type: "set", doc: { nodes: [text("a", 0, "<p>mine</p>")], edges: [] } }));
  await act(async () => vi.advanceTimersByTimeAsync(1600));
  expect(out.sync.conflict).not.toBeNull();
  update.mockImplementationOnce(async (_id, b) => pageOf(b.canvas!, 3));
  await act(async () => out.sync.keepMine());
  expect(update).toHaveBeenCalledTimes(2);
  expect(update.mock.calls[1][1]).toMatchObject({ canvas_version: 2, canvas: { nodes: [{ id: "a", html: "<p>mine</p>" }] } });
  await unmount();
});

it("takes someone else's newer canvas as it is when nothing is unsaved, and ignores an older one", async () => {
  const base = { nodes: [text("a")], edges: [] };
  const { qc, out, render, unmount } = mount(pageOf(base, 2));
  await render();
  qc.setQueryData(qk.wikiPage("c1"), pageOf({ nodes: [], edges: [] }, 1));
  await render();
  expect(out.doc.nodes.map((n) => n.id)).toEqual(["a"]);
  qc.setQueryData(qk.wikiPage("c1"), pageOf({ nodes: [text("z")], edges: [] }, 3));
  await render();
  expect(out.doc.nodes.map((n) => n.id)).toEqual(["z"]);
  expect(update).not.toHaveBeenCalled();
  await unmount();
});
