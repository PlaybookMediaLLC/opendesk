import { h, render } from "preact";
import { describe, expect, it } from "vitest";
import { ActionCard } from "./messages";
import type { AIAction } from "./types";

describe("action card", () => {
  it("shows the arguments the tool runs with, not only the agent's summary", () => {
    const a: AIAction = {
      id: "a1",
      title: "Refund order",
      summary: "Check the status of order 1042",
      status: "pending",
      tool: "Shop: refund",
      fields: [
        { label: "order", value: "1042" },
        { label: "Amount", value: "500" },
      ],
      omitted: 2,
    };
    const el = document.createElement("div");
    render(h(ActionCard, { a, ui: undefined, onDecide: () => {} }), el);
    const rows = [...el.querySelectorAll(".action-fields > div")].map((d) => [d.querySelector("dt")?.textContent, d.querySelector("dd")?.textContent]);
    expect(rows).toEqual([
      ["order", "1042"],
      ["Amount", "500"],
      ["…", "+2"],
    ]);
  });

  it("draws no list for a card without arguments", () => {
    const el = document.createElement("div");
    render(h(ActionCard, { a: { id: "a2", title: "Ping", summary: "", status: "pending", tool: "Shop: ping" }, ui: undefined, onDecide: () => {} }), el);
    expect(el.querySelector(".action-fields")).toBeNull();
  });
});
