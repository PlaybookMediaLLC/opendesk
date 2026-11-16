import { beforeEach, describe, expect, it, vi } from "vitest";

const openExternal = vi.fn();
vi.mock("electron", () => ({
  app: { isPackaged: true, getPath: () => "/tmp/opendesk-test" },
  BrowserWindow: class {},
  clipboard: {},
  Menu: {},
  nativeTheme: {},
  screen: {},
  net: { fetch: vi.fn() },
  shell: { openExternal: (...a: unknown[]) => openExternal(...a) },
}));

const { isAllowed, openOutside } = await import("./window");

beforeEach(() => openExternal.mockReset().mockResolvedValue(undefined));

describe("openOutside", () => {
  it("opens https and mailto in the default browser", () => {
    openOutside("https://example.com/docs");
    openOutside("mailto:ana@example.com");
    expect(openExternal.mock.calls.map((c) => c[0])).toEqual(["https://example.com/docs", "mailto:ana@example.com"]);
  });

  it("never opens other schemes, credentials or plain http in a packaged build", () => {
    for (const url of ["javascript:alert(1)", "file:///etc/passwd", "http://example.com/", "smb://host/share", "opendesk://open?path=/", "https://u:p@example.com/", "ms-msdt:/id", `mailto:${"a".repeat(3000)}@x.test`]) {
      openOutside(url);
    }
    expect(openExternal).not.toHaveBeenCalled();
  });
});

describe("isAllowed (in-window navigation)", () => {
  const instance = { appOrigin: "https://acme.example.com", apiOrigin: "https://api.acme.example.com" };
  it("stays on the instance's app and API origins", () => {
    expect(isAllowed("https://acme.example.com/inbox", instance)).toBe(true);
    expect(isAllowed("https://api.acme.example.com/v1/auth/oauth/google/start", instance)).toBe(true);
    for (const url of ["https://acme.example.com.evil.example/", "http://acme.example.com/", "https://evil.example/", "javascript:alert(1)", "not a url", "https://acme.example.com:8443/"]) {
      expect(isAllowed(url, instance), url).toBe(false);
    }
  });
});
