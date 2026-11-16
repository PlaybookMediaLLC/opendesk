import { describe, expect, it, vi } from "vitest";

vi.mock("electron", () => ({ app: { isPackaged: true }, net: { fetch: vi.fn() } }));

const { linkInArgs, openPath, parseLink } = await import("./links");

const APP = "https://acme.example.com";

describe("parseLink", () => {
  it("reads the sign-in callback and keeps its query for finishOAuth", () => {
    const l = parseLink("opendesk://auth/callback?code=abc");
    expect(l?.kind).toBe("auth");
    expect(l?.kind === "auth" && l.url.searchParams.get("code")).toBe("abc");
    expect(parseLink("OPENDESK://auth/callback/?error=denied")?.kind).toBe("auth");
  });

  it("reads open links and their path", () => {
    expect(parseLink("opendesk://open?path=/tickets/42")).toEqual({ kind: "open", path: "/tickets/42" });
    expect(parseLink("opendesk://open/")).toEqual({ kind: "open", path: null });
  });

  it("refuses what is not an opendesk:// link, and over-long ones", () => {
    for (const raw of ["https://evil.example/auth/callback", "javascript:alert(1)", "opendeskx://open", "", 42, null, `opendesk://open?path=/${"a".repeat(5000)}`]) {
      expect(parseLink(raw)).toBeNull();
    }
  });

  it("knows other hosts only as 'show the app'", () => {
    expect(parseLink("opendesk://settings")).toEqual({ kind: "other" });
    expect(parseLink("opendesk://auth/callbackx")).toEqual({ kind: "other" });
    expect(parseLink("opendesk://evil.example/auth/callback")).toEqual({ kind: "other" });
  });
});

describe("openPath", () => {
  const path = (p: string) => openPath(parseLink(`opendesk://open?path=${encodeURIComponent(p)}`)!, APP);

  it("passes same-origin app paths", () => {
    expect(path("/tickets/42?tab=notes#m1")).toBe("/tickets/42?tab=notes#m1");
    expect(path("/wiki/%2F%2Fevil.example")).toBe("/wiki/%2F%2Fevil.example");
  });

  it("refuses paths that would leave the instance", () => {
    for (const p of ["//evil.example/x", "/\\evil.example", "https://evil.example/", "javascript:alert(1)", "tickets/1", "/a\nb", "/a\u0000", ""]) {
      expect(path(p), p).toBeNull();
    }
    expect(openPath({ kind: "open", path: null }, APP)).toBeNull();
    expect(openPath({ kind: "other" }, APP)).toBeNull();
  });
});

describe("linkInArgs", () => {
  it("finds the link a second launch carries", () => {
    expect(linkInArgs(["/usr/bin/opendesk", "--flag", "opendesk://open?path=/x"])).toBe("opendesk://open?path=/x");
    expect(linkInArgs(["/usr/bin/opendesk", "https://opendesk.example"])).toBeUndefined();
  });
});
