// opendesk:// links (contracts/native.md, "Desktop shell"): what a link asks for, read
// before anything acts on it. Only two shapes do anything: opendesk://auth/callback (a
// native OAuth sign-in coming back) and opendesk://open?path=… (an app path, checked
// against the instance's origin by appPath before the window goes there).

import { PROTOCOL } from "./env";
import { appPath } from "./origins";

export const MAX_LINK = 4096;

export type Link = { kind: "auth"; url: URL } | { kind: "open"; path: string | null } | { kind: "other" };

/** Reads an opendesk:// link; null for anything that isn't one. */
export function parseLink(raw: unknown): Link | null {
  if (typeof raw !== "string" || raw.length > MAX_LINK || !raw.toLowerCase().startsWith(`${PROTOCOL}://`)) return null;
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return null;
  }
  const target = `${u.hostname}${u.pathname}`.replace(/\/+$/, "");
  if (target === "auth/callback") return { kind: "auth", url: u };
  if (target === "open") return { kind: "open", path: u.searchParams.get("path") };
  return { kind: "other" };
}

/** The app path an opendesk://open link may take the instance window to, or null. */
export function openPath(link: Link, appOrigin: string): string | null {
  return link.kind === "open" ? appPath(link.path, appOrigin) : null;
}

/** The opendesk:// link among a second launch's arguments, if any. */
export function linkInArgs(argv: readonly string[]): string | undefined {
  return argv.find((a) => a.toLowerCase().startsWith(`${PROTOCOL}://`));
}
