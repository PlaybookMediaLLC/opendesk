import { describe, expect, it } from "vitest";
import { canUseMailbox, mentionableIds, rowUsable } from "./access";

const base = { can_act: true, shares: { everyone: false, user_ids: ["bea"] }, thread: { assignees: [{ id: "cid", name: "Cid", email: "c@x.test", avatar_url: null }] }, account: { owner_id: "ada" } };

describe("collab access", () => {
  it("follows can_use_mailbox, and can_act from an older server", () => {
    expect(canUseMailbox({ can_act: true, can_use_mailbox: false })).toBe(false);
    expect(canUseMailbox({ can_act: true, can_use_mailbox: true })).toBe(true);
    expect(canUseMailbox({ can_act: true })).toBe(true);
    expect(canUseMailbox({ can_act: false, can_use_mailbox: false })).toBe(false);
  });

  it("limits mentions to people who see the conversation when its mailbox is someone else's", () => {
    expect(mentionableIds({ ...base, can_use_mailbox: true })).toBeNull();
    expect(mentionableIds({ ...base, can_use_mailbox: false, shares: { everyone: true, user_ids: [] } })).toBeNull();
    expect([...(mentionableIds({ ...base, can_use_mailbox: false }) ?? [])].sort()).toEqual(["ada", "bea", "cid"]);
  });

  it("acts on a private account's row only from its own accounts", () => {
    const mine = new Set(["acc-1"]);
    expect(rowUsable({ account_id: "acc-2", private_account: false }, mine)).toBe(true);
    expect(rowUsable({ account_id: "acc-2", private_account: true }, mine)).toBe(false);
    expect(rowUsable({ account_id: "acc-1", private_account: true }, mine)).toBe(true);
    expect(rowUsable({ account_id: "acc-2" }, mine)).toBe(true);
  });
});
