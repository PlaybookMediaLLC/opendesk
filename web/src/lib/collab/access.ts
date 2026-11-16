// Who uses a conversation's mailbox (contracts/collab.md "Who sees what"). A shared
// account's mailbox is everyone's who sees the conversation and can act; a private
// account's is its owner's alone. Others who see one of its conversations (shared on its
// own, or assigned) read it and comment: the controls that act at the provider, reply,
// forward, share, assign or mention someone new are not offered to them.

import type { MailThread, MailThreadDetail } from "@/lib/api/types";

type DetailAccess = Pick<MailThreadDetail, "can_act" | "can_use_mailbox">;

/** Whether you may act on the conversation at the provider, reply or forward from its account, save drafts, share it and assign it. Older servers send can_act alone. */
export function canUseMailbox(d: DetailAccess): boolean {
  return d.can_use_mailbox ?? d.can_act;
}

/** Who you may mention in the conversation: null when anyone may be (mentioning shares it), else only the people who see it already: its account's owner, its own shares and its assignees. */
export function mentionableIds(
  d: DetailAccess & { shares: Pick<MailThreadDetail["shares"], "everyone" | "user_ids">; thread: Pick<MailThreadDetail["thread"], "assignees">; account: Pick<MailThreadDetail["account"], "owner_id"> },
): Set<string> | null {
  if (canUseMailbox(d) || d.shares.everyone) return null;
  const ids = new Set<string>(d.shares.user_ids);
  for (const a of d.thread.assignees) ids.add(a.id);
  if (d.account.owner_id) ids.add(d.account.owner_id);
  return ids;
}

/** Whether a list row's conversation takes actions from you: one of a private account is yours to act on only when the account is (it is among your accounts). */
export function rowUsable(t: Pick<MailThread, "account_id" | "private_account">, accountIds: ReadonlySet<string>): boolean {
  return !t.private_account || accountIds.has(t.account_id);
}
