import { createHash } from "node:crypto";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const openExternal = vi.fn<(url: string, opts?: unknown) => Promise<void>>();
vi.mock("electron", () => ({ app: { isPackaged: true }, shell: { openExternal: (u: string, o?: unknown) => openExternal(u, o) } }));

const { cancelOAuth, finishOAuth, pkce, startOAuth } = await import("./oauth");

const API = "https://api.acme.example.com";
const START = `${API}/v1/auth/oauth/google/start?redirect_to=%2Finbox`;
const b64url = (b: Buffer) => b.toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

beforeEach(() => openExternal.mockReset().mockResolvedValue(undefined));
afterEach(() => cancelOAuth());

describe("pkce", () => {
  it("makes a verifier the contract allows and its S256 challenge", () => {
    const { verifier, challenge } = pkce();
    expect(verifier).toMatch(/^[A-Za-z0-9_-]{43,128}$/);
    expect(challenge).toBe(b64url(createHash("sha256").update(verifier).digest()));
    expect(pkce().verifier).not.toBe(verifier);
  });
});

describe("startOAuth", () => {
  it("opens only the instance API's start route, in the default browser", async () => {
    for (const bad of [
      "https://evil.example/v1/auth/oauth/google/start",
      `${API}/v1/auth/oauth/google/start#x`,
      `${API}/v1/auth/oauth/google/callback`,
      `${API}/v1/auth/oauth/../../admin/start`,
      `http://api.acme.example.com/v1/auth/oauth/google/start`,
      "javascript:alert(1)",
      42,
    ]) {
      expect(await startOAuth(bad, API), String(bad)).toEqual({ error: "invalid_request" });
    }
    expect(openExternal).not.toHaveBeenCalled();
  });

  it("adds the native client and the challenge, and hands back the code with the verifier", async () => {
    const flow = startOAuth(START, API);
    await Promise.resolve();
    expect(openExternal).toHaveBeenCalledTimes(1);
    const opened = new URL(openExternal.mock.calls[0][0]);
    expect(opened.origin + opened.pathname).toBe(`${API}/v1/auth/oauth/google/start`);
    expect(opened.searchParams.get("client")).toBe("native");
    expect(opened.searchParams.get("redirect_to")).toBe("/inbox");
    const challenge = opened.searchParams.get("code_challenge")!;

    expect(finishOAuth(new URL("opendesk://auth/callback?code=one-time.code_1"))).toBe(true);
    const r = await flow;
    expect(r).toMatchObject({ code: "one-time.code_1" });
    const verifier = (r as { verifier: string }).verifier;
    expect(b64url(createHash("sha256").update(verifier).digest())).toBe(challenge);
    // The flow is spent: a second callback finds nothing waiting.
    expect(finishOAuth(new URL("opendesk://auth/callback?code=again"))).toBe(false);
  });

  it("passes on a provider's error, and only a plain error code", async () => {
    let flow = startOAuth(START, API);
    finishOAuth(new URL("opendesk://auth/callback?error=access_denied"));
    expect(await flow).toEqual({ error: "access_denied" });
    flow = startOAuth(START, API);
    finishOAuth(new URL("opendesk://auth/callback?error=%3Cscript%3E"));
    expect(await flow).toEqual({ error: "invalid_request" });
    flow = startOAuth(START, API);
    finishOAuth(new URL("opendesk://auth/callback?code=has%20space"));
    expect(await flow).toEqual({ error: "invalid_request" });
  });

  it("cancels a flow in progress when another starts, and says when the browser can't open", async () => {
    const first = startOAuth(START, API);
    const second = startOAuth(START, API);
    expect(await first).toEqual({ error: "cancelled" });
    cancelOAuth();
    expect(await second).toEqual({ error: "cancelled" });
    openExternal.mockRejectedValueOnce(new Error("no browser"));
    expect(await startOAuth(START, API)).toEqual({ error: "browser_unavailable" });
  });

  it("ignores a callback nobody is waiting for", () => {
    expect(finishOAuth(new URL("opendesk://auth/callback?code=x"))).toBe(false);
  });
});
