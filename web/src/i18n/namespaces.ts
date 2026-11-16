// The namespaces of the catalogs (locales/<lng>/<ns>.json). Kept apart from the English
// catalogs themselves (./resources), so naming them never bundles them.

export const NAMESPACES = ["common", "billing", "collab", "ai", "agent", "agentQuality", "bots", "aiSetup", "aiTicket", "assistant", "public", "shell", "settings", "settingsInbox", "settingsDev", "settingsChannels", "settingsTeams", "emailDesign", "contacts", "kb", "mailCategories", "reports", "inbox", "indicators", "navigation", "auth", "ui", "imports", "setup", "shortcuts", "spaces", "wiki", "workspaces"] as const;
export type Namespace = (typeof NAMESPACES)[number];
