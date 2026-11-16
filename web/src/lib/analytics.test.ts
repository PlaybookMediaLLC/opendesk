import { describe, expect, it } from "vitest";
import { hasSecretParams, maskText, REPLAY_BLOCK, sanitize } from "./analytics";

describe("analytics never sees tokens", () => {
  it("knows which URLs carry secrets", () => {
    expect(hasSecretParams("?token=abc")).toBe(true);
    expect(hasSecretParams("?code=x&state=y")).toBe(true);
    expect(hasSecretParams("?ws=123&tab=open")).toBe(false);
  });
  it("drops queries from URL properties, person properties included", () => {
    const out = sanitize({
      $current_url: "https://app.test/reset?token=secret",
      $session_entry_url: "https://app.test/invite?token=secret#x",
      $set_once: { $initial_current_url: "https://app.test/verify?token=secret" },
      plan: "pro",
    });
    expect(JSON.stringify(out)).not.toContain("secret");
    expect(out.plan).toBe("pro");
  });
});

describe("session replay masks by default", () => {
  const html = (s: string) => {
    const host = document.createElement("div");
    host.innerHTML = s;
    return host;
  };

  it("masks text with no marker, and inside data-ph-mask", () => {
    const host = html(`<p id="plain">Refund for Ana</p><div data-ph-unmask><span data-ph-mask><b id="inner">secret</b></span></div>`);
    expect(maskText("Refund for Ana", host.querySelector("#plain"))).toBe("****** *** ***");
    expect(maskText("secret", host.querySelector("#inner"))).toBe("******");
    expect(maskText("no element")).toBe("** *******");
  });

  it("keeps the app's own labels under data-ph-unmask", () => {
    const host = html(`<nav data-ph-unmask><a><span id="label">Settings</span></a></nav><div class="ph-no-capture"><i id="nc">x y</i></div>`);
    expect(maskText("Settings", host.querySelector("#label"))).toBe("Settings");
    expect(maskText("x y", host.querySelector("#nc"))).toBe("* *");
  });

  it("leaves pictures, video and frames out of replays", () => {
    const host = html(`<img><video></video><iframe></iframe><p></p>`);
    expect(host.querySelectorAll(REPLAY_BLOCK)).toHaveLength(3);
  });
});
