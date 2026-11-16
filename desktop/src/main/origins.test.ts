import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const fetch = vi.fn();
let packaged = true;
vi.mock("electron", () => ({
  app: {
    get isPackaged() {
      return packaged;
    },
  },
  net: { fetch: (...a: unknown[]) => fetch(...a) },
}));

async function load(isPackaged = true) {
  packaged = isPackaged;
  vi.resetModules();
  return import("./origins");
}

beforeEach(() => fetch.mockReset());
afterEach(() => {
  packaged = true;
});

describe("externalUrl (what the default browser may open)", () => {
  it("allows https only in a packaged build, never with credentials", async () => {
    const { externalUrl } = await load();
    expect(externalUrl("https://example.com/a?b=1")).toBe("https://example.com/a?b=1");
    for (const v of [
      "http://example.com/",
      "http://localhost:3000/",
      "javascript:alert(1)",
      "file:///etc/passwd",
      "data:text/html,<b>x</b>",
      "opendesk://open?path=/",
      "smb://host/share",
      "https://user:pass@example.com/",
      "https://user@example.com/",
      "not a url",
      `https://example.com/${"a".repeat(9000)}`,
      42,
      null,
    ]) {
      expect(externalUrl(v), String(v)).toBeNull();
    }
  });

  it("allows http to local hosts in a development build only", async () => {
    const { externalUrl } = await load(false);
    expect(externalUrl("http://localhost:5173/")).toBe("http://localhost:5173/");
    expect(externalUrl("http://192.168.1.4/")).toBe("http://192.168.1.4/");
    expect(externalUrl("http://example.com/")).toBeNull();
  });
});

describe("appPath", () => {
  const APP = "https://acme.example.com";
  it("keeps same-origin absolute paths and refuses the rest", async () => {
    const { appPath } = await load();
    expect(appPath("/inbox?x=1#y", APP)).toBe("/inbox?x=1#y");
    for (const v of ["//evil.example", "/\\evil", "https://acme.example.com/inbox", "inbox", "/a\tb", "/a\u007f", "/" + "a".repeat(2050), 1, undefined]) {
      expect(appPath(v, APP), String(v)).toBeNull();
    }
  });
});

describe("isLocalHost", () => {
  it("knows loopback, private and .local addresses", async () => {
    const { isLocalHost } = await load();
    for (const h of ["localhost", "app.localhost", "printer.local", "127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.0.10", "[::1]", "fd00::1"]) expect(isLocalHost(h), h).toBe(true);
    for (const h of ["example.com", "172.32.0.1", "8.8.8.8", "localhost.example.com", "::ffff:8.8.8.8"]) expect(isLocalHost(h), h).toBe(false);
  });
});

describe("normalizeAddress", () => {
  it("turns what people type into an https origin; opendesk.sh means Cloud", async () => {
    const { normalizeAddress } = await load();
    expect(normalizeAddress("acme.example.com")).toEqual({ origin: "https://acme.example.com" });
    expect(normalizeAddress(" https://acme.example.com/inbox?x=1 ")).toEqual({ origin: "https://acme.example.com" });
    expect(normalizeAddress("opendesk.sh")).toEqual({ origin: "https://app.opendesk.sh" });
    expect(normalizeAddress("http://app.opendesk.sh")).toEqual({ origin: "https://app.opendesk.sh" });
  });

  it("refuses credentials, other schemes, bare words and http in a packaged build", async () => {
    const { normalizeAddress } = await load();
    expect(normalizeAddress("")).toEqual({ error: "empty" });
    expect(normalizeAddress("https://user:pw@acme.example.com")).toEqual({ error: "invalid" });
    expect(normalizeAddress("ftp://acme.example.com")).toEqual({ error: "invalid" });
    expect(normalizeAddress("javascript:alert(1)")).toEqual({ error: "invalid" });
    expect(normalizeAddress("intranet")).toEqual({ error: "invalid" });
    expect(normalizeAddress("acme example.com")).toEqual({ error: "invalid" });
    expect(normalizeAddress("http://acme.example.com")).toEqual({ error: "insecure" });
    expect(normalizeAddress("http://localhost:8080")).toEqual({ error: "insecure" });
  });

  it("allows http to a local host in a development build", async () => {
    const { normalizeAddress } = await load(false);
    expect(normalizeAddress("http://localhost:8080")).toEqual({ origin: "http://localhost:8080" });
  });
});

describe("resolveInstance", () => {
  const ORIGIN = "https://acme.example.com";
  const answer = (body: unknown, init: { status?: number; url?: string } = {}) => ({
    ok: (init.status ?? 200) < 300,
    url: init.url ?? `${ORIGIN}/config.json`,
    text: async () => (typeof body === "string" ? body : JSON.stringify(body)),
  });

  it("reads the API origin from config.json, the app origin when it is empty", async () => {
    const { resolveInstance } = await load();
    fetch.mockResolvedValueOnce(answer({ apiUrl: "https://api.acme.example.com/" }));
    expect(await resolveInstance(ORIGIN)).toEqual({ instance: { appOrigin: ORIGIN, apiOrigin: "https://api.acme.example.com" } });
    fetch.mockResolvedValueOnce(answer({ apiUrl: "" }));
    expect(await resolveInstance(ORIGIN)).toEqual({ instance: { appOrigin: ORIGIN, apiOrigin: ORIGIN } });
    expect(fetch.mock.calls[0][1]).toMatchObject({ credentials: "omit", cache: "no-store" });
  });

  it("refuses a redirect elsewhere, an http API and what isn't opendesk", async () => {
    const { resolveInstance } = await load();
    fetch.mockResolvedValueOnce(answer({}, { url: "https://evil.example/config.json" }));
    expect(await resolveInstance(ORIGIN)).toEqual({ error: "redirected" });
    fetch.mockResolvedValueOnce(answer({ apiUrl: "http://api.acme.example.com" }));
    expect(await resolveInstance(ORIGIN)).toEqual({ error: "insecure" });
    fetch.mockResolvedValueOnce(answer({ apiUrl: 5 }));
    expect(await resolveInstance(ORIGIN)).toEqual({ error: "not_opendesk" });
    fetch.mockResolvedValueOnce(answer("<html>", { status: 404 }));
    expect(await resolveInstance(ORIGIN)).toEqual({ error: "not_opendesk" });
    fetch.mockResolvedValueOnce(answer("[]"));
    expect(await resolveInstance(ORIGIN)).toEqual({ error: "not_opendesk" });
    fetch.mockRejectedValueOnce(new Error("net::ERR_CERT_AUTHORITY_INVALID"));
    expect(await resolveInstance(ORIGIN)).toEqual({ error: "certificate" });
    fetch.mockRejectedValueOnce(new Error("net::ERR_NAME_NOT_RESOLVED"));
    expect(await resolveInstance(ORIGIN)).toEqual({ error: "unreachable" });
  });

  it("takes a page without config.json as the app and API in lenient mode", async () => {
    const { resolveInstance } = await load();
    fetch.mockResolvedValueOnce(answer("nope", { status: 404 }));
    expect(await resolveInstance(ORIGIN, { lenient: true })).toEqual({ instance: { appOrigin: ORIGIN, apiOrigin: ORIGIN } });
  });
});
