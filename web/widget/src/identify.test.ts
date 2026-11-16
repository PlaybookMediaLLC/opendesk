import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Controller, otherPerson } from "./controller";
import { createMock, demoConfig } from "./mock";

describe("otherPerson", () => {
  it("is another user_id, or another email where neither has one", () => {
    expect(otherPerson({ user_id: "u1" }, { user_id: "u2" })).toBe(true);
    expect(otherPerson({ user_id: "u1" }, { user_id: "u1", email: "a@x.test" })).toBe(false);
    expect(otherPerson({ user_id: "u1" }, { name: "Ana" })).toBe(false);
    expect(otherPerson({}, { user_id: "u1" })).toBe(false);
    expect(otherPerson({ email: "a@x.test" }, { email: "A@X.test " })).toBe(false);
    expect(otherPerson({ email: "a@x.test" }, { email: "b@x.test" })).toBe(true);
    expect(otherPerson({ user_id: "u1", email: "a@x.test" }, { email: "b@x.test" })).toBe(false);
  });
});

describe("identify", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("starts over without the previous visitor token when another person signs in", async () => {
    const mock = createMock({ seed: "demo", latency: 5 });
    const calls: { method: string; path: string; auth: string | null; body: Record<string, unknown> | null }[] = [];
    const fetcher = (url: string, init: RequestInit) => {
      const headers = (init.headers ?? {}) as Record<string, string>;
      calls.push({
        method: init.method ?? "GET",
        path: new URL(url).pathname,
        auth: headers.Authorization ?? null,
        body: typeof init.body === "string" ? (JSON.parse(init.body) as Record<string, unknown>) : null,
      });
      return mock.fetch(url, init);
    };
    const c = new Controller({ base: "https://mock.invalid", key: "wk_demo", fetcher, socketFactory: mock.socket, persist: false });
    void c.identify({ user_id: "ana", user_hash: "h1", email: "ana@x.test" });
    const boot = c.boot(demoConfig("wk_demo"));
    await vi.advanceTimersByTimeAsync(200);
    await boot;
    const first = c.api.visitorToken;
    expect(first).toBeTruthy();

    // The same person with more details keeps the session.
    const more = c.identify({ user_id: "ana", name: "Ana" });
    await vi.advanceTimersByTimeAsync(200);
    await more;
    const same = calls.filter((x) => x.path === "/v1/widget/session" && x.method === "POST").at(-1)!;
    expect(same.body).toMatchObject({ visitor_token: first, user_id: "ana", email: "ana@x.test", name: "Ana" });

    // Another person: the old session ends, and the new one is asked for without its token or details.
    calls.length = 0;
    const next = c.identify({ user_id: "ben", user_hash: "h2" });
    await vi.advanceTimersByTimeAsync(200);
    await next;
    const ended = calls.find((x) => x.method === "DELETE" && x.path === "/v1/widget/session");
    expect(ended?.auth).toBe(`Visitor ${first}`);
    const started = calls.find((x) => x.method === "POST" && x.path === "/v1/widget/session")!;
    expect(started.body?.visitor_token).toBeUndefined();
    expect(started.body).toMatchObject({ user_id: "ben", user_hash: "h2" });
    expect(started.body?.email).toBeUndefined();
    c.stop();
    mock.dispose();
  });
});
