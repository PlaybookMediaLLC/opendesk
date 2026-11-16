// Saving a canvas and taking other people's (contracts/wiki.md "Canvas"). The canvas is
// replaced whole: changes are saved a moment after they stop, and every save names the
// canvas_version this tab edited from, so a save never writes over a canvas someone else
// saved since (409 canvas_changed). Someone else's canvas (from wiki_page.updated, a refetch,
// or a refused save) is taken as it is when this tab has nothing unsaved, and merged card by
// card when it has (./merge): the merge saves itself unless both changed the same card, which
// asks "keep mine" or "load theirs", as a page does. Nothing is taken mid-gesture.

import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState, type Dispatch } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "@/components/ui/toast";
import { ApiError, bindWorkspace, isApiError } from "@/lib/api/client";
import { api } from "@/lib/api/endpoints";
import type { Canvas, UserRef, WikiPage } from "@/lib/api/types";
import { qk } from "@/lib/queries";
import { mergeCanvas } from "./merge";
import { canonical, fromApi, toApi, type HistoryAction } from "./model";

const SAVE_AFTER = 700;
const RETRY_AFTER = 5000;

/** A canvas as the server holds it: what this tab's changes are made from. */
type Stored = { doc: Canvas; key: string; version: number | undefined; by: UserRef | null };

/** Two cards or connections both sides changed: the person picks a side. */
export type CanvasConflict = { by: UserRef | null; count: number };

const stored = (p: Pick<WikiPage, "canvas" | "canvas_version" | "updated_by">): Stored => {
  const doc = fromApi(p.canvas);
  return { doc, key: canonical(doc), version: p.canvas_version, by: p.updated_by ?? null };
};

/** The PATCH body for a canvas made from `from`. */
const saveBody = (body: Canvas, from: Stored) => ({ canvas: body, ...(from.version !== undefined ? { canvas_version: from.version } : {}) });

export function useCanvasSync(page: WikiPage, canEdit: boolean, doc: Canvas, dispatch: Dispatch<HistoryAction>) {
  const qc = useQueryClient();
  const { t } = useTranslation("wiki");
  const id = page.id;
  // Every request and cache key of this canvas stays in the workspace it opened in, after any await.
  const here = useRef(bindWorkspace()).current;
  const base = useRef<Stored | null>(null);
  if (base.current === null) base.current = stored(page);
  /** The canvas this tab last sent, or last took from the server. */
  const sent = useRef(base.current.key);
  const docRef = useRef(doc);
  docRef.current = doc;
  const busy = useRef(false);
  /** Someone else's canvas, waiting for this tab to be idle. */
  const pending = useRef<Stored | null>(null);
  const inflight = useRef(false);
  const again = useRef(false);
  const stopped = useRef(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const [conflict, setConflictState] = useState<CanvasConflict | null>(null);
  const conflictRef = useRef<CanvasConflict | null>(null);
  const setConflict = useCallback((c: CanvasConflict | null) => {
    conflictRef.current = c;
    setConflictState(c);
  }, []);

  const dirty = useCallback(() => canonical(docRef.current) !== sent.current, []);
  /** The latest save, for timers set before it was made. */
  const saveRef = useRef<() => Promise<void>>(async () => {});
  const schedule = useCallback((after = SAVE_AFTER) => {
    clearTimeout(timer.current);
    timer.current = setTimeout(() => void saveRef.current(), after);
  }, []);

  /** Puts a canvas on the board (clearing this tab's undo), at once for a save that follows before React draws it. */
  const show = useCallback(
    (next: Canvas) => {
      docRef.current = next;
      dispatch({ type: "reset", doc: next });
    },
    [dispatch],
  );

  /** Takes someone else's canvas: as it is with nothing unsaved, merged with this tab's changes otherwise. */
  const integrate = useCallback(() => {
    const theirs = pending.current;
    if (!theirs || busy.current || inflight.current || conflictRef.current) return;
    pending.current = null;
    const prev = base.current!;
    if (theirs.version !== undefined && prev.version !== undefined && theirs.version <= prev.version) return;
    // Without versions (an older server), this tab's own save coming back is told by its cards.
    if (theirs.version === undefined && (theirs.key === prev.key || theirs.key === sent.current)) {
      base.current = { ...theirs, version: prev.version };
      return;
    }
    base.current = theirs;
    const mine = docRef.current;
    if (!dirty()) {
      sent.current = theirs.key;
      if (theirs.key !== canonical(mine)) show(theirs.doc);
      return;
    }
    const merged = mergeCanvas(prev.doc, mine, theirs.doc);
    // What is unsaved now is what the merge adds to theirs.
    sent.current = theirs.key;
    if (canonical(merged.doc) !== canonical(mine)) show(merged.doc);
    if (merged.conflicts.length) setConflict({ by: theirs.by, count: merged.conflicts.length });
    else if (canonical(merged.doc) !== theirs.key) schedule(0);
  }, [dirty, schedule, setConflict, show]);

  /** Hands someone else's canvas over, newest first. */
  const offer = useCallback(
    (theirs: Stored) => {
      const cur = pending.current;
      if (!cur || cur.version === undefined || theirs.version === undefined || theirs.version >= cur.version) pending.current = theirs;
      integrate();
    },
    [integrate],
  );

  const save = useCallback(async () => {
    clearTimeout(timer.current);
    if (!canEdit || stopped.current || conflictRef.current) return;
    if (inflight.current) {
      again.current = true;
      return;
    }
    const body = toApi(docRef.current);
    const key = canonical(body);
    if (key === sent.current) return;
    inflight.current = true;
    sent.current = key;
    const from = base.current!;
    try {
      const saved = await here(() => api.wiki.update(id, saveBody(body, from)));
      const now = stored(saved);
      base.current = { ...now, version: now.version ?? from.version };
      here(() =>
        qc.setQueryData<WikiPage>(qk.wikiPage(id), (old) =>
          old ? { ...old, canvas: saved.canvas, canvas_version: saved.canvas_version, updated_at: saved.updated_at, updated_by: saved.updated_by } : old,
        ),
      );
    } catch (e) {
      const status = e instanceof ApiError ? e.status : 0;
      if (isApiError(e, "canvas_changed")) {
        // Someone else saved first: their canvas is read and merged with this tab's changes.
        sent.current = from.key;
        try {
          const fresh = await here(() => api.wiki.get(id));
          here(() => qc.setQueryData<WikiPage>(qk.wikiPage(id), fresh));
          const theirs = stored(fresh);
          const cur = pending.current;
          if (!cur || (cur.version ?? 0) <= (theirs.version ?? 0)) pending.current = theirs;
        } catch (err) {
          toast.error(err, t("canvas.saveFailed"));
          schedule(RETRY_AFTER);
        }
      } else if (status === 403 || status === 404) {
        // The page is gone or no longer this person's to edit: saving stops for good.
        stopped.current = true;
        toast.error(e, t("canvas.saveStopped"));
      } else if (status === 422 || status === 413) {
        // Refused as it is: the next change tries again.
        toast.error(e, t("canvas.saveFailed"));
      } else {
        sent.current = base.current!.key;
        toast.error(e, t("canvas.saveFailed"));
        schedule(RETRY_AFTER);
      }
    } finally {
      inflight.current = false;
      integrate();
      if (again.current) {
        again.current = false;
        void saveRef.current();
      }
    }
  }, [canEdit, here, id, integrate, qc, schedule, t]);
  saveRef.current = save;

  // Save a moment after the canvas stops changing.
  useEffect(() => {
    if (!canEdit || stopped.current || conflictRef.current || !dirty()) return;
    schedule();
  }, [doc, canEdit, dirty, schedule]);

  // Someone else's canvas on the cached page (an event, a refetch).
  useEffect(() => {
    offer(stored(page));
    // The page's last editor rides along only to name them in a conflict.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page.canvas, page.canvas_version, offer]);

  /** Marks a gesture or an edit under way (true) or over (false): someone else's canvas waits for it. */
  const setBusy = useCallback(
    (on: boolean) => {
      busy.current = on;
      if (!on) integrate();
    },
    [integrate],
  );

  /** Saves the merge as it shows, this tab's side of every conflict. */
  const keepMine = useCallback(() => {
    setConflict(null);
    void saveRef.current();
  }, [setConflict]);

  /** Drops this tab's unsaved changes for the stored canvas. */
  const loadTheirs = useCallback(() => {
    const theirs = base.current!;
    setConflict(null);
    sent.current = theirs.key;
    show(theirs.doc);
    integrate();
  }, [integrate, setConflict, show]);

  // Whatever is unsaved goes when the canvas closes or the tab does. Closing in the app merges
  // with a canvas saved since, this tab's side kept where both changed a card; a tab that unloads
  // sends what it has with its version (the browser asks first), and never writes over a newer one.
  useEffect(() => {
    const unsaved = () => canEdit && !stopped.current && dirty();
    const beforeUnload = (e: BeforeUnloadEvent) => {
      if (!unsaved()) return;
      void here(() => api.wiki.update(id, saveBody(toApi(docRef.current), base.current!), { keepalive: true })).catch(() => {});
      e.preventDefault();
    };
    window.addEventListener("beforeunload", beforeUnload);
    return () => {
      window.removeEventListener("beforeunload", beforeUnload);
      clearTimeout(timer.current);
      if (!unsaved()) return;
      const body = toApi(docRef.current);
      const from = base.current!;
      sent.current = canonical(body);
      void here(() => api.wiki.update(id, saveBody(body, from)))
        .catch(async (e: unknown) => {
          if (!isApiError(e, "canvas_changed")) return;
          const fresh = await here(() => api.wiki.get(id));
          const theirs = stored(fresh);
          const merged = mergeCanvas(from.doc, body, theirs.doc).doc;
          if (canonical(merged) !== theirs.key) await here(() => api.wiki.update(id, saveBody(toApi(merged), theirs)));
        })
        .catch(() => {});
    };
  }, [canEdit, dirty, here, id]);

  return { setBusy, saveNow: save, conflict, keepMine, loadTheirs };
}
