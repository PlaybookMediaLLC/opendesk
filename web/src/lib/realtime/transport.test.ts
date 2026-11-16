// The realtime token rides in Sec-WebSocket-Protocol and is read again at every connect:
// phoenix.js takes authToken through closure(), which calls a function each time. A
// phoenix release that stopped doing so would send a stale token or none: this pins it.
import { afterEach, describe, expect, it } from "vitest";
import { createSocket } from "./transport";

const PREFIX = "base64url.bearer.phx.";
const opened: { url: string; protocols: string[] }[] = [];

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  readyState = 0;
  binaryType = "arraybuffer";
  onopen: (() => void) | null = null;
  onclose: ((e: unknown) => void) | null = null;
  onerror: ((e: unknown) => void) | null = null;
  onmessage: ((e: unknown) => void) | null = null;
  constructor(url: string, protocols?: string | string[]) {
    opened.push({ url, protocols: Array.isArray(protocols) ? protocols : protocols ? [protocols] : [] });
  }
  close() {
    this.readyState = 3;
  }
  send() {}
}

const real = globalThis.WebSocket;
afterEach(() => {
  globalThis.WebSocket = real;
  opened.length = 0;
});

const tokenOf = (protocols: string[]) => {
  const p = protocols.find((x) => x.startsWith(PREFIX));
  return p ? atob(p.slice(PREFIX.length).replace(/-/g, "+").replace(/_/g, "/")) : null;
};

describe("realtime transport", () => {
  it("sends the current token in the WebSocket protocol at every connect, never in the URL", () => {
    globalThis.WebSocket = FakeWebSocket as unknown as typeof WebSocket;
    let token = "tok-1";
    const s = createSocket("wss://gateway.example/socket", () => ({ token, client: "web" }));
    s.connect();
    s.disconnect();
    token = "tok-2";
    s.connect();
    s.disconnect();
    expect(opened).toHaveLength(2);
    expect(opened.map((o) => tokenOf(o.protocols))).toEqual(["tok-1", "tok-2"]);
    for (const o of opened) {
      expect(o.url).not.toContain("tok-");
      expect(o.url).toContain("client=web");
    }
  });
});
