// The agent surfaces: which tools each role's assistant actually has.
// Tools are defined HERE, next to the session and its authorization —
// every closure is bound to the signed-in user, so a tool can only ever
// read or stage what that person could see or do by hand. The loop
// itself (model calls, validation, receipts) lives in kernel/ai.go.
package webapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"counterfoil/internal/core"
	"counterfoil/internal/kernel"
)

// ---------------------------------------------------------------- leaver chat

const leaverSystem = `You are Counterfoil's assistant inside an HR exits product, chatting with %s (employee id %s). Today is %s.
Hard rules:
- Answer ONLY from tool results. If a tool does not return a fact, say you cannot confirm it. Never invent dates, amounts or statuses.
- Repeat figures EXACTLY as the tool gives them (prefer the *_display fields); never recompute, merge or swap lines.
- If a follow-up needs figures or details you have not already stated verbatim, call the tool again — do not claim data is unavailable without re-checking.
- Policy answers carry their citation (the "cite" field) in brackets.
- If they express an intent to resign, call propose_resignation (pass their date only if they stated one, format YYYY-MM-DD). Then tell them a confirmation card has appeared below: nothing is submitted until they press Confirm, and nothing is accepted until their named approver decides.
- You cannot decide, approve, submit, cancel or change anything. Named people decide; you read, draft and stage.
- Reply in 1-4 short, plain sentences. Amounts are INR. Do not use markdown.`

// leaverTools builds the leaver's toolset, closed over their session.
func (s *Server) leaverTools(u core.User, emp core.Employee) []kernel.AITool {
	return []kernel.AITool{
		{
			Name: "get_policy",
			Desc: "Look up one HR policy topic in the cited rule pack (notice period, final settlement timing, gratuity, leave encashment, relieving letter, resignation process, buyout).",
			Params: map[string]kernel.AIParam{
				"query": {Type: "string", Desc: "the topic or question, in a few words"},
			},
			Required: []string{"query"},
			Run: func(args map[string]any) (map[string]any, error) {
				q, _ := args["query"].(string)
				if qa, ok := s.app.Rules().AnswerQA(q); ok {
					return map[string]any{"answer": qa.Answer, "cite": qa.Cite,
						"summary": "cited " + qa.Cite}, nil
				}
				return map[string]any{"found": false,
					"note": "no matching clause in the rule pack; say you cannot confirm this and suggest asking HR operations"}, nil
			},
		},
		{
			Name: "get_my_exit_status",
			Desc: "The employee's own exit case as it stands now: steps, approvals, clocks, anything in doubt. Call this for any 'where is my…' or status question.",
			Run: func(map[string]any) (map[string]any, error) {
				c, ok := s.app.CaseForEmployee(emp.ID)
				if !ok {
					return map[string]any{"has_case": false,
						"note":    "no exit case is open; they can tender a resignation in this chat",
						"summary": "no open case"}, nil
				}
				defs := s.app.StepDefs(c)
				done, total := 0, 0
				var waitingOn []string
				for _, def := range defs {
					total++
					st := c.Steps[def.ID]
					if st == nil {
						continue
					}
					switch st.Status {
					case core.StepDone, core.StepSkipped:
						done++
					case core.StepWaiting, core.StepRunning, core.StepInDoubt:
						if len(waitingOn) < 4 {
							waitingOn = append(waitingOn, fmt.Sprintf("%s (%s)", def.Title, st.Status))
						}
					}
				}
				var clocks []map[string]any
				for _, ob := range s.app.ObligationsFor(c.ID) {
					clocks = append(clocks, map[string]any{
						"what": ob.Title, "due": ob.DueDate, "status": string(ob.Status), "basis": ob.Basis})
				}
				indoubt := 0
				for _, it := range s.app.InDoubt(true) {
					if it.CaseID == c.ID {
						indoubt++
					}
				}
				out := map[string]any{
					"has_case": true, "case_id": c.ID, "status": string(c.Status),
					"tender_date": c.TenderDate, "requested_last_working_day": c.RequestedLWD,
					"steps_done": done, "steps_total": total, "clocks": clocks,
					"open_in_doubt_items": indoubt,
					"summary":             fmt.Sprintf("%s: %d/%d steps done", c.ID, done, total),
				}
				if c.BoundLWD != "" {
					out["last_working_day"] = c.BoundLWD
					out["last_working_day_note"] = "fixed by the approver's mandate"
				} else {
					out["last_working_day"] = "not yet decided — awaiting the named approver"
				}
				if len(waitingOn) > 0 {
					out["currently_waiting_on"] = waitingOn
				}
				return out, nil
			},
		},
		{
			Name: "get_my_settlement",
			Desc: "The employee's final settlement (full-and-final): each line with its amount and citation, the net payable, and whether the payment has actually been RELEASED or is still held.",
			Run: func(map[string]any) (map[string]any, error) {
				c, ok := s.app.CaseForEmployee(emp.ID)
				if !ok {
					return map[string]any{"available": false, "note": "no exit case is open"}, nil
				}
				ffRaw, ok := c.Facts["ff"]
				if !ok {
					return map[string]any{"available": false,
						"note":    "not computed yet — the settlement is prepared near the last working day, after clearances",
						"summary": "not yet computed"}, nil
				}
				b, err := json.Marshal(ffRaw)
				if err != nil {
					return nil, err
				}
				// Decode with the kernel's own type — its JSON tags are
				// the ones the receipts actually carry (amount_inr,
				// total_inr); a hand-rolled struct here once read zeros.
				var ff kernel.FFFacts
				if err := json.Unmarshal(b, &ff); err != nil {
					return nil, err
				}
				// Figures are given as preformatted strings as well as
				// numbers: a model relays "₹1,06,666.67" more faithfully
				// than it reads nested JSON.
				var lines []map[string]any
				var display []string
				for _, l := range ff.Lines {
					lines = append(lines, map[string]any{"label": l.Label, "amount_inr": l.Amount, "cite": l.Cite})
					display = append(display, fmt.Sprintf("%s: ₹%.2f (%s)", l.Label, l.Amount, l.Cite))
				}
				out := map[string]any{
					"available": true, "period": ff.Period,
					"net_payable_inr":     ff.Total,
					"net_payable_display": fmt.Sprintf("₹%.2f", ff.Total),
					"lines":               lines,
					"lines_display":       display,
					"released":            false,
					"note":                "net_payable is the sum of the lines; a negative line is a recovery",
					"summary":             fmt.Sprintf("net ₹%.2f over %d lines", ff.Total, len(lines)),
				}
				for _, rc := range s.app.Receipts(0, c.ID) {
					if rc.Action == "payment.released" {
						out["released"] = true
						out["release"] = fmt.Sprintf("released under mandate %v at evidence %s — receipt #%d",
							rc.Data["mandate_id"], rc.Evidence, rc.Seq)
						break
					}
				}
				if out["released"] == false {
					out["release"] = "not yet released: it stands as a held draft until the payroll approver signs off, and counts as paid only when payroll's own record confirms it"
				}
				return out, nil
			},
		},
		{
			Name: "propose_resignation",
			Desc: "Stage the employee's resignation for THEIR confirmation. This only shows a confirmation card — the employee's own Confirm click is the formal tender; this tool cannot submit anything.",
			Params: map[string]kernel.AIParam{
				"requested_last_working_day": {Type: "string", Desc: "YYYY-MM-DD, only if the employee stated a date"},
			},
			Run: func(args map[string]any) (map[string]any, error) {
				if _, ok := s.app.CaseForEmployee(emp.ID); ok {
					return nil, fmt.Errorf("an exit case is already open for this employee")
				}
				lwd, _ := args["requested_last_working_day"].(string)
				if lwd != "" {
					t, err := time.Parse("2006-01-02", lwd)
					if err != nil {
						return nil, fmt.Errorf("requested_last_working_day must be YYYY-MM-DD")
					}
					if t.Before(time.Now().Truncate(24 * time.Hour)) {
						return nil, fmt.Errorf("requested_last_working_day %s is in the past", lwd)
					}
				}
				setPending(u.ID, pendingIntake{Active: true, RequestedLWD: lwd})
				disp := lwd
				if disp == "" {
					disp = "not stated (the notice rule proposes the earliest date)"
				}
				return map[string]any{
					"staged": true, "requested_last_working_day": disp,
					"note":    "a confirmation card is now visible below this chat; nothing is submitted until the employee presses Confirm, and acceptance is decided by a named approver",
					"summary": "confirmation card shown",
				}, nil
			},
		},
	}
}

// agentSrc renders the honest source chip from what actually ran.
func agentSrc(model string, steps []kernel.AgentStep) string {
	var tools []string
	for _, st := range steps {
		if st.Kind != "tool" {
			continue
		}
		mark := " ✓"
		if !st.OK {
			mark = " ✗"
		}
		tools = append(tools, st.Name+mark)
	}
	src := "agent · " + model
	if len(tools) > 0 {
		src += " · " + strings.Join(tools, ", ")
	} else {
		src += " · no tools used"
	}
	return src
}

// tryAgentChat routes one leaver message through the tool-calling agent.
// It returns true when the agent answered (messages added); false sends
// the caller down the rules path, with the error for an honest note.
func (s *Server) tryAgentChat(u core.User, emp core.Employee, caseID, text string) (bool, error) {
	ai := s.app.Router().AI()
	if ai == nil {
		return false, nil
	}
	history := kernel.HistoryFromChat(s.app.ChatFor(caseID), 12)
	// The current message is passed separately; drop it from history.
	if n := len(history); n > 0 && history[n-1].Role == "user" && history[n-1].Text == text {
		history = history[:n-1]
	}
	system := fmt.Sprintf(leaverSystem, emp.Name, emp.ID, time.Now().Format("Monday, 2 January 2006"))
	final, steps, err := ai.RunAgent("leaver chat agent", system, history, text, s.leaverTools(u, emp), 6)
	if err != nil {
		return false, err
	}
	s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil", Text: final,
		Kind: "src:" + agentSrc(ai.Model(), steps)})
	return true, nil
}

// ---------------------------------------------------------------- in-doubt draft

const investigatorSystem = `You draft an investigation brief for an HR operations officer about ONE in-doubt item: an external action whose outcome could not be established.
Method: first call get_item_details. Then call read_target_record to see what the target system holds RIGHT NOW. Call get_case_receipts only if the order of events matters. Then stop calling tools and write your findings as short plain text.
Rules: report only what the tools returned, and name the evidence level. A success reply alone proves nothing — only the target's own record counts. If the step is reserved, the only lawful resolution is re-dispatch under its mandate (retry). You draft; a named person decides.`

type inDoubtDraft struct {
	ItemID      string
	Summary     string
	TargetHolds string
	Rec         string
	Reasoning   string
	Caution     string
	Steps       []kernel.AgentStep
	Model       string
	By          string
	At          time.Time
	Err         string // non-empty = the draft failed; shown as-is
}

var draftsByItem = struct {
	m     map[string]*inDoubtDraft
	guard chan struct{}
}{m: map[string]*inDoubtDraft{}, guard: make(chan struct{}, 1)}

func setDraft(id string, d *inDoubtDraft) {
	draftsByItem.guard <- struct{}{}
	draftsByItem.m[id] = d
	<-draftsByItem.guard
}

func getDraft(id string) *inDoubtDraft {
	draftsByItem.guard <- struct{}{}
	d := draftsByItem.m[id]
	<-draftsByItem.guard
	return d
}

// investigationTools are read-only. Each derives WHAT to read from the
// item id fixed at request time — the model chooses when to look, never
// where. No tool here can resolve, retry or change anything.
func (s *Server) investigationTools(itemID string) []kernel.AITool {
	return []kernel.AITool{
		{
			Name: "get_item_details",
			Desc: "What the platform itself knows about this in-doubt item: the step's declaration, the intent ledger entry, whether the step is reserved.",
			Run: func(map[string]any) (map[string]any, error) {
				return s.app.InDoubtDetail(itemID)
			},
		},
		{
			Name: "read_target_record",
			Desc: "Perform a fresh read of the target system's own records for this item's request reference, with the dispatcher's production read-back logic. Returns the verdict, the evidence level and the raw records found.",
			Run: func(map[string]any) (map[string]any, error) {
				return s.app.ReadTargetForItem(itemID)
			},
		},
		{
			Name: "get_case_receipts",
			Desc: "The case's receipt trail, condensed: sequence, action, actor and notes — the order of what actually happened.",
			Run: func(map[string]any) (map[string]any, error) {
				it, ok := s.app.InDoubtItemByID(itemID)
				if !ok {
					return nil, fmt.Errorf("item not found")
				}
				return map[string]any{"receipts": s.app.ReceiptSummaries(it.CaseID, 30)}, nil
			},
		},
	}
}

// draftInvestigation runs the two-phase draft: a bounded tool loop to
// gather real evidence, then a schema-constrained JSON call for the
// typed recommendation. For reserved steps the schema's enum is just
// ["retry"] — the draft cannot even phrase a forbidden resolution.
func (s *Server) draftInvestigation(u core.User, itemID string) *inDoubtDraft {
	d := &inDoubtDraft{ItemID: itemID, By: u.Name, At: time.Now()}
	ai := s.app.Router().AI()
	if ai == nil {
		d.Err = "no model configured"
		return d
	}
	d.Model = ai.Model()
	it, ok := s.app.InDoubtItemByID(itemID)
	if !ok {
		d.Err = "item not found"
		return d
	}
	findings, steps, err := ai.RunAgent("in-doubt investigation draft", investigatorSystem,
		nil, fmt.Sprintf("Investigate in-doubt item %s (case %s, step %s). Reported reason: %s",
			it.ID, it.CaseID, it.StepID, it.Reason),
		s.investigationTools(itemID), 6)
	d.Steps = steps
	if err != nil {
		d.Err = err.Error()
		return d
	}
	resolutions := []string{"retry", "confirmed_done", "cancelled"}
	reserved := false
	if def, ok := s.app.StepDefFor(it.CaseID, it.StepID); ok && def.Reserved != "" {
		resolutions = []string{"retry"}
		reserved = true
	}
	schema := map[string]any{
		"type": "OBJECT",
		"properties": map[string]any{
			"summary":                map[string]any{"type": "STRING", "description": "two sentences: what happened and what the target holds"},
			"what_the_target_holds":  map[string]any{"type": "STRING"},
			"recommended_resolution": map[string]any{"type": "STRING", "enum": resolutions},
			"reasoning":              map[string]any{"type": "STRING", "description": "one or two sentences, grounded in the evidence"},
			"caution":                map[string]any{"type": "STRING", "description": "what the officer should verify by hand before acting"},
		},
		"required": []string{"summary", "recommended_resolution", "reasoning"},
	}
	userMsg := fmt.Sprintf("Item %s. Findings from the investigation:\n%s", it.ID, findings)
	if reserved {
		userMsg += "\nThis step is RESERVED: re-dispatch under its mandate (retry) is the only possible resolution."
	}
	out, err := ai.GenerateJSON("in-doubt draft recommendation", investigatorSystem, userMsg, schema)
	if err != nil {
		d.Err = err.Error()
		return d
	}
	str := func(k string) string { v, _ := out[k].(string); return strings.TrimSpace(v) }
	d.Summary, d.TargetHolds = str("summary"), str("what_the_target_holds")
	d.Rec, d.Reasoning, d.Caution = str("recommended_resolution"), str("reasoning"), str("caution")
	return d
}
