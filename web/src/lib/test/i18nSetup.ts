// Every test reads the app's strings in English, as the agent app does once its catalogs
// are in (src/i18n/index.ts: only English `common` is bundled; the rest load at boot).
// Loading a catalog adds its namespace to those i18next loads on a change of language;
// the list goes back to what boot has, so a test of a public page loads what that page does.
import { i18n, loadAgentCatalogs } from "@/i18n";

const ns = [...((i18n.options.ns as string[] | undefined) ?? ["common"])];
await loadAgentCatalogs("en");
i18n.options.ns = ns;
