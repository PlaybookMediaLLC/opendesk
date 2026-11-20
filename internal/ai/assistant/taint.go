package assistant

// Outside text (contracts/ai.md "Approvals"): once a conversation holds text written outside
// the workspace (a customer's message or subject, a contact's name, a teammate's mail, a
// connected tool's answer), its changes and every connected tool call wait for the user's
// approval, whatever the workspace lets run without asking. The model chooses arguments
// after reading that text, so a person sees them before they run or leave the workspace.

// insideTools are the built-in tools whose answers hold only what the team or the model
// itself wrote: the API description, saved views, labels and fields, members, the knowledge
// base and the wiki, and the confirmations of changes that name only a ticket number or
// what the model wrote. Every other tool's answer counts as outside text.
var insideTools = map[string]bool{
	"find_api":                true,
	"list_views":              true,
	"list_labels_and_fields":  true,
	"list_team":               true,
	"search_kb":               true,
	"search_wiki":             true,
	"get_wiki_page":           true,
	"add_note":                true,
	"draft_reply":             true,
	"bulk_update_tickets":     true,
	"create_kb_draft":         true,
	"create_view":             true,
	"create_automation_draft": true,
}

// bringsOutsideText reports whether a tool's answer counts as outside text.
func bringsOutsideText(name string) bool { return !insideTools[name] }

// outsideText reports whether a conversation's stored turns hold outside text: a message
// sent from an open ticket (its page context describes the ticket), or a call of a tool whose
// answer counts as outside text.
func outsideText(ts []turn) bool {
	for _, t := range ts {
		if t.Body.Context != nil && t.Body.Context.TicketID != "" {
			return true
		}
		for _, c := range t.Body.Calls {
			if bringsOutsideText(c.Name) {
				return true
			}
		}
	}
	return false
}

// needsApproval reports whether a call is proposed for the user's approval rather than run
// as a read: every write, and, once the conversation holds outside text, every connected
// tool, reads included, since its arguments leave the workspace.
func (rc *runCtx) needsApproval(t *tool) bool {
	return t.Write || (rc.outside && t.ext != nil)
}
