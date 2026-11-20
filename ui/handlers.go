package webapp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"counterfoil/internal/audit"
	"counterfoil/internal/core"
	"counterfoil/internal/kernel"
)

func (s *Server) routes() {
	m := s.mux
	m.Handle("GET /static/", http.FileServerFS(staticFS))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })

	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.login)
	m.HandleFunc("GET /logout", s.logout)
	m.HandleFunc("GET /{$}", s.home)

	m.HandleFunc("GET /chat", s.chatPage)
	m.HandleFunc("POST /chat/send", s.chatSend)
	m.HandleFunc("POST /chat/confirm", s.chatConfirm)
	m.HandleFunc("GET /myexit", s.myexitPage)

	m.HandleFunc("GET /work", s.workPage)
	m.HandleFunc("GET /approvals/{id}", s.approvalPage)
	m.HandleFunc("POST /approvals/{id}/decide", s.approvalDecide)
	m.HandleFunc("POST /tasks/{id}/done", s.taskDone)
	m.HandleFunc("POST /tasks/{id}/skip", s.taskSkip)

	m.HandleFunc("GET /cases", s.casesPage)
	m.HandleFunc("GET /cases/{id}", s.casePage)
	m.HandleFunc("GET /indoubt", s.indoubtPage)
	m.HandleFunc("POST /indoubt/{id}/resolve", s.indoubtResolve)
	m.HandleFunc("POST /indoubt/{id}/draft", s.indoubtDraft)

	m.HandleFunc("GET /dashboard", s.dashboardPage)

	m.HandleFunc("GET /admin", s.adminPage)
	m.HandleFunc("POST /admin/kill", s.adminKill)
	m.HandleFunc("POST /admin/lie", s.adminLie)
	m.HandleFunc("POST /admin/tamper", s.adminTamper)
	m.HandleFunc("POST /admin/canaries", s.adminCanaries)
	m.HandleFunc("POST /admin/pkg/check", s.adminPkgCheck)
	m.HandleFunc("POST /admin/pkg/activate", s.adminPkgActivate)
	m.HandleFunc("POST /admin/reserved/add", s.adminReservedAdd)

	m.HandleFunc("GET /audit", s.auditPage)
	m.HandleFunc("POST /audit/export", s.auditExport)
	m.HandleFunc("POST /audit/verify", s.auditVerify)
	m.HandleFunc("GET /letter/{id}", s.letterPage)

	m.HandleFunc("GET /receipts/{seq}", s.receiptPage)
	m.HandleFunc("GET /people", s.peoplePage)
	m.HandleFunc("GET /people/{id}", s.personPage)
	m.HandleFunc("GET /clocks", s.clocksPage)
	m.HandleFunc("GET /settlement/{id}", s.settlementPage)
	m.HandleFunc("GET /doctrine", s.doctrinePage)

	m.HandleFunc("GET /onboarding/new", s.onboardingNewPage)
	m.HandleFunc("POST /onboarding/start", s.onboardingStart)
	m.HandleFunc("GET /survey/{id}", s.surveyPage)
	m.HandleFunc("POST /survey/{id}", s.surveySubmit)
	m.HandleFunc("GET /search", s.searchPage)
	m.HandleFunc("GET /mandates", s.mandatesPage)
	m.HandleFunc("GET /about", s.aboutPage)
	m.HandleFunc("GET /security", s.securityPage)
	m.HandleFunc("GET /privacy", s.privacyPage)
	m.HandleFunc("GET /credits", s.creditsPage)
	m.HandleFunc("/", s.notFound)
}

// ---------------------------------------------------------------- login

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	type vm struct {
		Users []core.User
		Tape  []core.Receipt
		Seq   int64
		Head  string
	}
	users := s.app.Users()
	order := map[string]int{"leaver": 0, "manager": 1, "payroll": 2, "hrops": 3, "hradmin": 4, "auditor": 5}
	sort.SliceStable(users, func(i, j int) bool { return order[users[i].Role] < order[users[j].Role] })
	v := vm{Users: users}
	// The ledger, live: the login proves the product before anyone signs
	// in — the last few receipts and the chain head, real, not staged.
	all := s.app.Receipts(0, "")
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	if n := len(all); n > 6 {
		all = all[n-6:]
	}
	v.Tape = all
	v.Seq, v.Head = s.app.Chain().Head()
	s.render(w, r, "login", "", "Sign in", v)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	uid := r.FormValue("user")
	if _, ok := s.app.User(uid); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	sid := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[sid] = uid
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "cfoil_sid", Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	u, _ := s.app.User(uid)
	http.Redirect(w, r, homeFor(u), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	delete(s.sessions, s.sid(r))
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "cfoil_sid", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	u, ok := s.user(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, homeFor(u), http.StatusSeeOther)
}

// ---------------------------------------------------------------- chat (the leaver)

type chatVM struct {
	Emp       core.Employee
	Msgs      []kernel.ChatMsg
	Case      *core.Case
	StepsDone int
	StepsAll  int
	Pending   pendingIntake
}

type pendingIntake struct {
	Active       bool
	RequestedLWD string
}

var pendingByUser = struct {
	sync  map[string]pendingIntake
	guard chan struct{}
}{sync: map[string]pendingIntake{}, guard: make(chan struct{}, 1)}

func setPending(uid string, p pendingIntake) {
	pendingByUser.guard <- struct{}{}
	pendingByUser.sync[uid] = p
	<-pendingByUser.guard
}

func getPending(uid string) pendingIntake {
	pendingByUser.guard <- struct{}{}
	p := pendingByUser.sync[uid]
	<-pendingByUser.guard
	return p
}

func (s *Server) chatPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "leaver")
	if !ok {
		return
	}
	emp, _ := s.app.Employee(u.EmpID)
	vm := chatVM{Emp: emp, Pending: getPending(u.ID)}
	if c, ok := s.app.CaseForEmployee(u.EmpID); ok {
		vm.Case = c
		vm.Msgs = s.app.ChatFor(c.ID)
		for _, st := range c.Steps {
			vm.StepsAll++
			if st.Status == core.StepDone || st.Status == core.StepSkipped {
				vm.StepsDone++
			}
		}
	} else {
		vm.Msgs = s.app.ChatFor("PRE-" + u.ID)
	}
	s.render(w, r, "chat", "chat", "Chat", vm)
}

// ---------------------------------------------------------------- my exit (the leaver's case room)

type myexitVM struct {
	C            caseVM
	Tasks        []*core.Task
	ClocksClosed int
	ClocksAll    int
	Attributed   int
	InDoubt      int
	ApproverName string
	Next         *stepVM
	NextWho      string
}

func (s *Server) myexitPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "leaver")
	if !ok {
		return
	}
	c, has := s.app.CaseForEmployee(u.EmpID)
	if !has {
		s.setFlash(r, "No exit case yet — your journey starts with a tender in the chat.")
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	v := myexitVM{C: s.buildCaseVM(c, true), Tasks: s.app.TasksFor("leaver", u.ID)}
	for _, cl := range v.C.Clocks {
		v.ClocksAll++
		if cl.O.Status == core.ClockClosed {
			v.ClocksClosed++
		}
	}
	v.Attributed = v.C.Coverage["attributed"]
	for _, it := range s.app.InDoubt(true) {
		if it.CaseID == c.ID {
			v.InDoubt++
		}
	}
	for _, rc := range s.app.Receipts(0, c.ID) {
		if rc.Action == "mandate.issued" {
			if m, ok := rc.Data["mandate"].(map[string]any); ok {
				if rid, _ := m["reserved_id"].(string); rid == "RSV-1" {
					if ap, _ := m["approver_id"].(string); ap != "" {
						if au, ok := s.app.User(ap); ok {
							v.ApproverName = au.Name
						}
					}
				}
			}
		}
	}
	if v.ApproverName == "" {
		v.ApproverName = "your approver"
	}
	// The first step that is not finished, and who moves it — read from
	// the step definitions, not a script.
	for i := range v.C.Steps {
		st := &v.C.Steps[i]
		if st.Status == core.StepDone || st.Status == core.StepSkipped {
			continue
		}
		v.Next = st
		switch {
		case st.Status == core.StepInDoubt:
			v.NextWho = "The outcome could not be established — a named person in HR operations is looking at the target system's own records."
		case st.Kind == core.StepApproval:
			v.NextWho = "Waits on the named approver's signature — never an agent, never a timeout."
		case st.Kind == core.StepHumanInput && st.Role == "leaver":
			v.NextWho = "Waits on you — it is in the list below."
		case st.Kind == core.StepHumanInput:
			v.NextWho = "Waits on the " + st.Role + " — it is on their work queue."
		case st.At == "lwd":
			v.NextWho = "Runs automatically at the last-working-day cut-off, then is read back against the target's own record."
		default:
			v.NextWho = "Runs automatically under the warrant path, then is read back."
		}
		break
	}
	s.render(w, r, "myexit", "myexit", "My exit", v)
}

func (s *Server) chatSend(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "leaver")
	if !ok {
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	caseID := "PRE-" + u.ID
	if c, ok := s.app.CaseForEmployee(u.EmpID); ok {
		caseID = c.ID
	}
	s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: u.ID, Text: text})
	emp, _ := s.app.Employee(u.EmpID)

	// Rules first — but intake detection only matters while no case is
	// open: once a case exists, every message (even one that mentions
	// notice or resignation) belongs to the agent, which has the
	// context; its staging tool refuses a second tender anyway.
	isRes, lwd := false, ""
	if strings.HasPrefix(caseID, "PRE-") {
		isRes, lwd = s.app.Router().ParseIntake(text)
	}
	agentNote := ""
	switch {
	case isRes:
		disp := "not stated (the notice rule will propose the earliest date)"
		if lwd != "" {
			disp = kernel.FDisplay(lwd)
		}
		ack, src := s.app.Router().Acknowledge(emp.Name, disp)
		s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil", Text: ack,
			Kind: "src:" + src})
		setPending(u.ID, pendingIntake{Active: true, RequestedLWD: lwd})
	default:
		// Everything else goes to the tool-calling agent when a model is
		// configured; it reads real case data through typed tools and can
		// itself stage a resignation the regex missed (same confirm card,
		// same named approver). On any failure: rules, saying so.
		if ok, err := s.tryAgentChat(u, emp, caseID, text); ok {
			break
		} else if err != nil {
			agentNote = " · agent unavailable, rules answered (" + err.Error() + ")"
		}
		if qa, ok := s.app.Rules().AnswerQA(text); ok {
			s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil",
				Text: qa.Answer, Kind: "cite:" + qa.Cite + agentNote})
		} else {
			s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil",
				Text: "I answer policy questions from the cited rule pack, and I can take your resignation. " +
					"Try: \"What is my notice period?\" — or tell me you wish to resign.",
				Kind: "src:rules (no matching clause)" + agentNote})
		}
	}
	http.Redirect(w, r, "/chat", http.StatusSeeOther)
}

func (s *Server) chatConfirm(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "leaver")
	if !ok {
		return
	}
	p := getPending(u.ID)
	if !p.Active {
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	if r.FormValue("decision") != "confirm" {
		setPending(u.ID, pendingIntake{})
		s.app.AddChat(kernel.ChatMsg{CaseID: "PRE-" + u.ID, From: "counterfoil",
			Text: "No problem — nothing has been submitted."})
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	if v := strings.TrimSpace(r.FormValue("requested_lwd")); v != "" {
		p.RequestedLWD = v
	}
	caseID, err := s.app.StartResignation(u.EmpID, p.RequestedLWD)
	setPending(u.ID, pendingIntake{})
	if err != nil {
		s.setFlash(r, err.Error())
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	// Carry the pre-case transcript into the case.
	for _, m := range s.app.ChatFor("PRE-" + u.ID) {
		m.CaseID = caseID
		s.app.AddChat(m)
	}
	s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil",
		Text: "Submitted. Your resignation is acknowledged and the case is open — nothing is accepted until your named approver decides. Your exit page now shows every step and clock; each line comes from a receipt."})
	http.Redirect(w, r, "/myexit", http.StatusSeeOther)
}

// ---------------------------------------------------------------- work (approvals + tasks)

type decisionRow struct {
	TS     time.Time
	CaseID string
	Status string
	Reason string
	RSV    string
}

type workVM struct {
	Approvals []*core.Approval
	CaseOf    map[string]*core.Case
	EmpOf     map[string]core.Employee
	Tasks     []*core.Task
	Decisions []decisionRow
}

func (s *Server) workPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	vm := workVM{CaseOf: map[string]*core.Case{}, EmpOf: map[string]core.Employee{}}
	vm.Approvals = s.app.ApprovalsFor(u.ID)
	vm.Tasks = s.app.TasksFor(u.Role, u.ID)
	for _, ap := range vm.Approvals {
		if c, ok := s.app.Case(ap.CaseID); ok {
			vm.CaseOf[ap.CaseID] = c
			if e, ok := s.app.Employee(c.EmployeeID); ok {
				vm.EmpOf[ap.CaseID] = e
			}
		}
	}
	// Their own decisions, newest first — an approver's personal ledger.
	// (Receipts returns newest-first.)
	for _, rc := range s.app.Receipts(0, "") {
		if len(vm.Decisions) >= 8 {
			break
		}
		if rc.Action != "approval.decided" || rc.ActorID != u.ID {
			continue
		}
		d := decisionRow{TS: rc.TS, CaseID: rc.CaseID}
		d.Status, _ = rc.Data["status"].(string)
		d.Reason, _ = rc.Data["reason"].(string)
		if req, ok := rc.Data["request"].(map[string]any); ok {
			if a, _ := req["action"].(string); a != "" {
				d.RSV = a
			}
		}
		vm.Decisions = append(vm.Decisions, d)
	}
	s.render(w, r, "work", "work", "Your work", vm)
}

type approvalVM struct {
	A        *core.Approval
	Case     *core.Case
	Emp      core.Employee
	Args     []kv
	Hash     string
	Editable bool
	Mandate  *core.Mandate
	Reserved kernel.ReservedDef
}

type kv struct{ K, V string }

func (s *Server) approvalPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	ap, ok := s.app.Approval(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if ap.ApproverID != u.ID && u.Role != "auditor" && u.Role != "hrops" {
		http.Error(w, "403: this approval belongs to its named approver", http.StatusForbidden)
		return
	}
	c, _ := s.app.Case(ap.CaseID)
	emp := core.Employee{}
	if c != nil {
		emp, _ = s.app.Employee(c.EmployeeID)
	}
	rdef, _ := s.app.ReservedByID(ap.ReservedID)
	vm := approvalVM{A: ap, Case: c, Emp: emp, Hash: core.MustHashCanon(ap.Request),
		Editable: ap.Status == core.ApprovalOpen && ap.ReservedID == "RSV-1" && ap.ApproverID == u.ID,
		Reserved: rdef}
	if args, ok := ap.Request["args"].(map[string]any); ok {
		keys := make([]string, 0, len(args))
		for k := range args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			vm.Args = append(vm.Args, kv{K: strings.ReplaceAll(k, "_", " "), V: fmt.Sprint(args[k])})
		}
	}
	s.render(w, r, "approval", "work", "Approval "+ap.ID, vm)
}

func (s *Server) approvalDecide(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	edits := map[string]any{}
	if v := r.FormValue("last_working_day"); v != "" {
		edits["last_working_day"] = v
	}
	if r.FormValue("edit_buyout") == "1" {
		edits["notice_buyout"] = r.FormValue("notice_buyout")
	}
	err := s.app.DecideApproval(id, u.ID, r.FormValue("pin"),
		r.FormValue("decision") == "approve", edits, r.FormValue("reason"))
	if err != nil {
		s.setFlash(r, err.Error())
	} else if r.FormValue("decision") == "approve" {
		s.setFlash(r, "Approved. The mandate binds exactly what you saw; the dispatcher re-checks the hash before anything leaves.")
	} else {
		s.setFlash(r, "Rejected and receipted.")
	}
	http.Redirect(w, r, "/approvals/"+id, http.StatusSeeOther)
}

// safeBack keeps redirects on this site: a same-site relative path only —
// parsed, no scheme, no host, a single leading slash, and no backslash or
// control character anywhere.
func safeBack(v string) string {
	for _, r := range v {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "/work"
		}
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/work"
	}
	return v
}

func (s *Server) taskDone(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.app.CompleteTask(r.PathValue("id"), u.ID, r.FormValue("note")); err != nil {
		s.setFlash(r, err.Error())
	}
	http.Redirect(w, r, safeBack(r.FormValue("back")), http.StatusSeeOther)
}

func (s *Server) taskSkip(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.app.SkipTask(r.PathValue("id"), u.ID); err != nil {
		s.setFlash(r, err.Error())
	}
	http.Redirect(w, r, safeBack(r.FormValue("back")), http.StatusSeeOther)
}

// ---------------------------------------------------------------- cases

type stepVM struct {
	N        int
	ID       string
	Title    string
	Kind     core.StepKind
	Reserved string
	Role     string
	At       string
	Status   core.StepStatus
	Actor    string
	Evidence string
	Note     string
	Human    bool
	Checked  bool
}

type clockVM struct {
	O        *core.Obligation
	Trace    string
	DaysLeft int
}

type caseVM struct {
	C         *core.Case
	Emp       core.Employee
	Steps     []stepVM
	Clocks    []clockVM
	InDoubt   []*core.InDoubtItem
	Receipts  []core.Receipt
	Auto      string // "n of m"
	AutoPct   string
	Coverage  map[string]int
	FF        *kernel.FFFacts
	StepsDone int
	StepsAll  int
}

func (s *Server) buildCaseVM(c *core.Case, forLeaver bool) caseVM {
	vm := caseVM{C: c, Coverage: map[string]int{}}
	vm.Emp, _ = s.app.Employee(c.EmployeeID)
	defs := s.app.StepDefs(c)
	doneAuto, doneAll := 0, 0
	for _, d := range defs {
		st := c.Steps[d.ID]
		if st == nil {
			st = &core.StepState{ID: d.ID, Status: core.StepPending}
		}
		sv := stepVM{N: d.N, ID: d.ID, Title: d.Title, Kind: d.Kind, Reserved: d.Reserved,
			Role: d.Role, At: d.At,
			Status: st.Status, Actor: st.Actor, Evidence: st.Evidence, Note: st.Note,
			Human:   d.Kind == core.StepApproval || d.Kind == core.StepHumanInput,
			Checked: st.Evidence == core.EvAttributed || st.Evidence == core.EvObserved}
		if u, ok := s.app.User(st.Actor); ok {
			sv.Actor = u.Name
		}
		vm.Steps = append(vm.Steps, sv)
		vm.StepsAll++
		if st.Status == core.StepDone || st.Status == core.StepSkipped {
			vm.StepsDone++
			doneAll++
			if !st.HumanTouch {
				doneAuto++
			}
		}
		if st.Evidence != "" && d.Effect != nil && d.Effect.Target != "internal" {
			vm.Coverage[st.Evidence]++
		}
	}
	vm.Auto = fmt.Sprintf("%d of %d", doneAuto, len(defs))
	if doneAll > 0 {
		vm.AutoPct = fmt.Sprintf("%.0f%%", 100*float64(doneAuto)/float64(len(defs)))
	}
	for _, ob := range s.app.ObligationsFor(c.ID) {
		cv := clockVM{O: ob}
		var tr []string
		tr = append(tr, fmt.Sprintf("start %s (%s)", kernel.FDisplay(ob.StartDate), ob.StartEvent))
		for _, sk := range ob.SkippedDays {
			tr = append(tr, "skip "+sk)
		}
		tr = append(tr, "due "+kernel.FDisplay(ob.DueDate))
		cv.Trace = strings.Join(tr, " → ")
		if d, err := time.Parse("2006-01-02", ob.DueDate); err == nil {
			cv.DaysLeft = int(time.Until(d).Hours()/24) + 1
		}
		vm.Clocks = append(vm.Clocks, cv)
	}
	for _, it := range s.app.InDoubt(false) {
		if it.CaseID == c.ID {
			vm.InDoubt = append(vm.InDoubt, it)
		}
	}
	limit := 400
	if forLeaver {
		limit = 0
	}
	all := s.app.Receipts(limit, c.ID)
	if forLeaver {
		// The passbook: receipts the leaver can read, no internals.
		keep := map[string]bool{"case.opened": true, "step.status": false,
			"separation.accepted": true, "obligation.started": true, "obligation.closed": true,
			"payment.released": true, "document.issued": true, "message.sent": true,
			"case.closed": true, "effect.readback": true}
		for _, rr := range all {
			if keep[rr.Action] {
				vm.Receipts = append(vm.Receipts, rr)
			}
		}
	} else {
		vm.Receipts = all
	}
	if ff, ok := c.Facts["ff"]; ok {
		if f, ok2 := kernel.AsFF(ff); ok2 {
			vm.FF = &f
		}
	}
	return vm
}

func (s *Server) casesPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "hradmin", "auditor", "manager", "payroll")
	if !ok {
		return
	}
	type row struct {
		C       *core.Case
		Emp     core.Employee
		Auto    string
		Open    int
		Done    int
		All     int
		Pct     int
		InDoubt int
	}
	var rows []row
	for _, c := range s.app.Cases() {
		if !s.canSeeCase(u, c) {
			continue
		}
		vm := s.buildCaseVM(c, false)
		open := 0
		for _, cl := range vm.Clocks {
			if cl.O.Status != core.ClockClosed {
				open++
			}
		}
		rw := row{C: c, Emp: vm.Emp, Auto: vm.Auto, Open: open, Done: vm.StepsDone, All: vm.StepsAll}
		if rw.All > 0 {
			rw.Pct = rw.Done * 100 / rw.All
		}
		for _, it := range s.app.InDoubt(true) {
			if it.CaseID == c.ID {
				rw.InDoubt++
			}
		}
		rows = append(rows, row{C: rw.C, Emp: rw.Emp, Auto: rw.Auto, Open: rw.Open,
			Done: rw.Done, All: rw.All, Pct: rw.Pct, InDoubt: rw.InDoubt})
	}
	s.render(w, r, "cases", "cases", "Cases", rows)
}

type participant struct {
	Who    string
	Name   string
	Detail string
	Pill   string // css pill suffix, "" = none
}

func (s *Server) casePage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "hradmin", "auditor", "manager", "payroll")
	if !ok {
		return
	}
	c, ok := s.app.Case(r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.canSeeCase(u, c) {
		http.Error(w, "403: a manager sees their own reports' cases", http.StatusForbidden)
		return
	}
	type vmT struct {
		caseVM
		Participants []participant
	}
	vm := vmT{caseVM: s.buildCaseVM(c, false)}
	// Who touched this case, read from the record: the leaver, their
	// manager, and every named approver with the state of their decision.
	vm.Participants = append(vm.Participants, participant{Who: "Leaver", Name: vm.Emp.Name,
		Detail: vm.Emp.ID + " · " + vm.Emp.Title})
	if mgr, ok := s.app.Employee(vm.Emp.ManagerID); ok {
		vm.Participants = append(vm.Participants, participant{Who: "Manager", Name: mgr.Name,
			Detail: mgr.ID + " · org chart"})
	}
	type apState struct{ approver, reserved, status string }
	aps := map[string]*apState{}
	var order []string
	all := s.app.Receipts(0, c.ID) // newest-first; walk oldest-first
	for i := len(all) - 1; i >= 0; i-- {
		rc := all[i]
		switch rc.Action {
		case "approval.created":
			if m, ok := rc.Data["approval"].(map[string]any); ok {
				id, _ := m["id"].(string)
				ap, _ := m["approver_id"].(string)
				rsv, _ := m["reserved_id"].(string)
				if id != "" {
					aps[id] = &apState{approver: ap, reserved: rsv, status: "open"}
					order = append(order, id)
				}
			}
		case "approval.decided":
			if id, _ := rc.Data["approval_id"].(string); id != "" {
				if st, ok := aps[id]; ok {
					st.status, _ = rc.Data["status"].(string)
				}
			}
		}
	}
	for _, id := range order {
		st := aps[id]
		name := st.approver
		if au, ok := s.app.User(st.approver); ok {
			name = au.Name
		}
		pill := "waiting"
		if st.status == "granted" {
			pill = "done"
		} else if st.status == "rejected" {
			pill = "breached"
		}
		vm.Participants = append(vm.Participants, participant{Who: "Approver · " + st.reserved,
			Name: name, Detail: id + " · " + st.status, Pill: pill})
	}
	s.render(w, r, "case", "cases", c.ID, vm)
}

// ---------------------------------------------------------------- in doubt

func (s *Server) indoubtPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "payroll", "hradmin", "auditor"); !ok {
		return
	}
	type vm struct {
		Open    []*core.InDoubtItem
		Closed  []*core.InDoubtItem
		Drafts  map[string]*inDoubtDraft
		HasAI   bool
		AIModel string
		Oldest  string
	}
	v := vm{Drafts: map[string]*inDoubtDraft{}}
	if ai := s.app.Router().AI(); ai != nil {
		v.HasAI, v.AIModel = true, ai.Model()
	}
	var oldest time.Time
	for _, it := range s.app.InDoubt(false) {
		if it.Open {
			v.Open = append(v.Open, it)
			if d := getDraft(it.ID); d != nil {
				v.Drafts[it.ID] = d
			}
			if oldest.IsZero() || it.Created.Before(oldest) {
				oldest = it.Created
			}
		} else {
			v.Closed = append(v.Closed, it)
		}
	}
	if !oldest.IsZero() {
		if age := time.Since(oldest); age < time.Hour {
			v.Oldest = fmt.Sprintf("%.0f min", age.Minutes())
		} else {
			v.Oldest = fmt.Sprintf("%.1f h", time.Since(oldest).Hours())
		}
	}
	s.render(w, r, "indoubt", "indoubt", "In doubt", v)
}

// indoubtDraft runs the investigation agent for one item and stores the
// draft for the page to show. Drafting is advisory: it reads, it cannot
// resolve — the resolve form stays the only way a person acts.
func (s *Server) indoubtDraft(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "payroll", "hradmin")
	if !ok {
		return
	}
	id := r.PathValue("id")
	if it, ok := s.app.InDoubtItemByID(id); !ok || !it.Open {
		s.setFlash(r, "no open in-doubt item "+id)
		http.Redirect(w, r, "/indoubt", http.StatusSeeOther)
		return
	}
	setDraft(id, s.draftInvestigation(u, id))
	http.Redirect(w, r, "/indoubt", http.StatusSeeOther)
}

func (s *Server) indoubtResolve(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "payroll")
	if !ok {
		return
	}
	err := s.app.ResolveInDoubt(r.PathValue("id"), u.ID, r.FormValue("resolution"), r.FormValue("note"))
	if err != nil {
		s.setFlash(r, err.Error())
	} else {
		s.setFlash(r, "Resolved and receipted.")
	}
	http.Redirect(w, r, "/indoubt", http.StatusSeeOther)
}

// ---------------------------------------------------------------- dashboard

type dashVM struct {
	CasesOpen, CasesClosed                                               int
	Auto                                                                 string
	AutoNote                                                             string
	Coverage                                                             map[string]int
	CoverageNote                                                         string
	ClocksOpen, ClocksAtRisk, ClocksClosed, ClocksBreached, ClocksOnTime int
	InDoubtOpen                                                          int
	Seq                                                                  int64
	Head                                                                 string
	Cp                                                                   core.Checkpoint
	SignerErr                                                            string
	ModelCalls                                                           int
	ModelTokens                                                          int64
	ModelName                                                            string
	Canaries                                                             int
	CanariesHeld                                                         int
	AvgClose                                                             string
	AvgResolve                                                           string
	Refusals                                                             int
	Retries                                                              int
	Feed                                                                 []core.Receipt
}

func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	// The org-wide feed and meters are a back-office view; the roles match
	// the sidebar that links here.
	if _, ok := s.requireUser(w, r, "hrops", "hradmin", "auditor"); !ok {
		return
	}
	v := dashVM{Coverage: map[string]int{}}
	autoDone, autoAll := 0, 0
	for _, c := range s.app.Cases() {
		if c.Status == core.CaseOpen {
			v.CasesOpen++
		} else {
			v.CasesClosed++
		}
		vm := s.buildCaseVM(c, false)
		for _, st := range vm.Steps {
			if st.Status == core.StepDone || st.Status == core.StepSkipped {
				autoAll++
				if !(st.Human && st.Status == core.StepDone) {
					// counted precisely below from receipts' human_touch
				}
			}
		}
		for k, n := range vm.Coverage {
			v.Coverage[k] += n
		}
		for _, cl := range vm.Clocks {
			switch cl.O.Status {
			case core.ClockOpen:
				v.ClocksOpen++
			case core.ClockAtRisk:
				v.ClocksAtRisk++
			case core.ClockBreached:
				v.ClocksBreached++
			case core.ClockClosed:
				v.ClocksClosed++
				if !cl.O.ClosedAt.IsZero() && cl.O.ClosedAt.Format("2006-01-02") <= cl.O.DueDate {
					v.ClocksOnTime++
				}
			}
		}
	}
	// Automation measured from receipts: steps done with no human touch.
	// The impact row is measured the same way — from this instance's own
	// receipts and timestamps, never asserted.
	humanDone, allDone := 0, 0
	for _, rr := range s.app.Receipts(0, "") {
		switch rr.Action {
		case "step.status":
			if st, _ := rr.Data["status"].(string); st == "done" || st == "skipped" {
				allDone++
				if ht, _ := rr.Data["human_touch"].(bool); ht {
					humanDone++
				}
			}
		case "effect.refused":
			v.Refusals++
		case "effect.retried":
			// Automatic (transport) retries; manual ones are counted via
			// their resolution receipt so older chains read true too.
			if rr.ActorType != "human" {
				v.Retries++
			}
		case "indoubt.resolved":
			if res, _ := rr.Data["resolution"].(string); res == "retry" {
				v.Retries++
			}
		}
	}
	var closeSum time.Duration
	closeN := 0
	for _, c := range s.app.Cases() {
		if c.Status == core.CaseClosed && !c.ClosedAt.IsZero() {
			closeSum += c.ClosedAt.Sub(c.OpenedAt)
			closeN++
		}
	}
	if closeN > 0 {
		v.AvgClose = fmt.Sprintf("%.1f days over %d case(s)", (closeSum/time.Duration(closeN)).Hours()/24, closeN)
	}
	var resSum time.Duration
	resN := 0
	for _, it := range s.app.InDoubt(false) {
		if !it.Open && !it.Resolved.IsZero() {
			resSum += it.Resolved.Sub(it.Created)
			resN++
		}
	}
	if resN > 0 {
		avg := resSum / time.Duration(resN)
		if avg < time.Hour {
			v.AvgResolve = fmt.Sprintf("%.0f min over %d item(s)", avg.Minutes(), resN)
		} else {
			v.AvgResolve = fmt.Sprintf("%.1f h over %d item(s)", avg.Hours(), resN)
		}
	}
	_ = autoDone
	_ = autoAll
	if allDone > 0 {
		v.Auto = fmt.Sprintf("%d of %d", allDone-humanDone, allDone)
		v.AutoNote = fmt.Sprintf("measured on this instance's receipts (design target: 15 of 19 per exit); %d completions, %d by a person", allDone, humanDone)
	} else {
		v.Auto = "no steps completed yet"
	}
	v.CoverageNote = "external effects by evidence level; reply-only means the target offered nothing to read back"
	v.InDoubtOpen = len(s.app.InDoubt(true))
	v.Seq, v.Head = s.app.Chain().Head()
	v.Cp, v.SignerErr = s.app.Chain().LastCheckpoint()
	v.ModelCalls, v.ModelTokens = s.app.ModelSpend()
	v.ModelName = s.app.Router().ModelName()
	v.Feed = s.app.Receipts(8, "")
	for _, c := range s.app.CanaryResults() {
		v.Canaries++
		if held, _ := c["held"].(bool); held {
			v.CanariesHeld++
		}
	}
	s.render(w, r, "dashboard", "dashboard", "Dashboard", v)
}

// ---------------------------------------------------------------- admin

type adminVM struct {
	Kill     kernel.KillSwitch
	Users    []core.User
	Packages []*kernel.Package
	Checks   map[string]kernel.CheckResult
	Canaries []kernel.CanaryResult
	Reserved []kernel.ReservedDef
	MocksURL string
	TamperOn bool
}

var lastChecks = map[string]kernel.CheckResult{}
var lastCanaries []kernel.CanaryResult

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "hradmin"); !ok {
		return
	}
	v := adminVM{Kill: s.app.Kill(), Users: s.app.Users(), Packages: s.app.Packages(),
		Checks: lastChecks, Canaries: lastCanaries, Reserved: s.app.ReservedList(),
		MocksURL: s.app.Config().MocksURL, TamperOn: s.app.TamperNext()}
	s.render(w, r, "admin", "admin", "Controls", v)
}

func (s *Server) adminKill(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "hradmin")
	if !ok {
		return
	}
	on := r.FormValue("on") == "1"
	_ = s.app.SetKillSwitch(u.ID, on, r.FormValue("scope"))
	if on {
		s.setFlash(r, "Kill switch ON: the next action is blocked. Clocks keep running — pausing agents never extends a legal deadline.")
	} else {
		s.setFlash(r, "Kill switch off; the engine resumes from the receipts.")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminLie(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "hradmin"); !ok {
		return
	}
	svc, on := r.FormValue("svc"), r.FormValue("on")
	switch svc {
	case "itsm", "directory", "cards", "payroll":
	default:
		s.setFlash(r, "unknown stand-in: "+svc)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if on != "1" {
		on = "0"
	}
	req, _ := http.NewRequest("POST", s.app.Config().MocksURL+"/admin/"+svc+"/lie?on="+on, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		s.setFlash(r, fmt.Sprintf("%s lie mode set to %s on the stand-in.", svc, on))
	} else {
		s.setFlash(r, "could not reach the stand-ins: "+err.Error())
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminTamper(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "hradmin"); !ok {
		return
	}
	s.app.SetTamperNext(r.FormValue("on") == "1")
	s.setFlash(r, "The next reserved dispatch will have ₹1 added after approval — the gate must refuse it.")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminCanaries(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "hradmin")
	if !ok {
		return
	}
	lastCanaries = s.app.RunCanaries(u.ID)
	held := 0
	for _, c := range lastCanaries {
		if c.Held {
			held++
		}
	}
	s.setFlash(r, fmt.Sprintf("Canaries: %d of %d held, each receipted.", held, len(lastCanaries)))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminPkgCheck(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "hradmin")
	if !ok {
		return
	}
	name := r.FormValue("pkg")
	res, err := s.app.RunCheck(name, u.ID)
	if err != nil {
		s.setFlash(r, err.Error())
	} else {
		lastChecks[name] = res
		if res.Errors == 0 {
			s.setFlash(r, fmt.Sprintf("cfoil check: PASS — 0 errors, %d warning(s). Certificate %s… (inactive until two named people activate it).", res.Warnings, core.Short(res.Cert)))
		} else {
			s.setFlash(r, fmt.Sprintf("cfoil check: REFUSED — %d error(s). Nothing is loaded.", res.Errors))
		}
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminPkgActivate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hradmin", "hrops")
	if !ok {
		return
	}
	name := r.FormValue("pkg")
	confirmer := r.FormValue("confirmer")
	cu, okc := s.app.User(confirmer)
	if !okc || cu.PIN != r.FormValue("confirmer_pin") {
		s.setFlash(r, "activation needs the second person's PIN")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if u2, _ := s.app.User(u.ID); u2.PIN != r.FormValue("pin") {
		s.setFlash(r, "re-authentication failed: wrong PIN")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if err := s.app.Activate(name, u.ID, confirmer); err != nil {
		s.setFlash(r, err.Error())
	} else {
		s.setFlash(r, name+" activated by two named people; receipted with its certificate. Zero core files changed.")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminReservedAdd(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hradmin")
	if !ok {
		return
	}
	def := kernel.ReservedDef{ID: strings.TrimSpace(r.FormValue("id")),
		Action:       strings.TrimSpace(r.FormValue("action")),
		Title:        strings.TrimSpace(r.FormValue("title")),
		ApproverRole: r.FormValue("approver_role")}
	if err := s.app.AddReserved(u.ID, def); err != nil {
		s.setFlash(r, err.Error())
	} else {
		s.setFlash(r, def.ID+" added to the reserved list (the floor itself can never be removed).")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ---------------------------------------------------------------- audit

type auditVM struct {
	Seq         int64
	Head        string
	Cp          core.Checkpoint
	SignerErr   string
	Receipts    []core.Receipt
	ExportDir   string
	Exported    bool
	Checks      []audit.Check
	CheckedAt   time.Time
	Checkpoints []core.Checkpoint
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "auditor", "hradmin", "hrops"); !ok {
		return
	}
	v := auditVM{Receipts: s.app.Receipts(250, r.URL.Query().Get("case")),
		ExportDir: s.exportDir}
	v.Seq, v.Head = s.app.Chain().Head()
	v.Cp, v.SignerErr = s.app.Chain().LastCheckpoint()
	v.Checkpoints = s.app.Chain().Checkpoints()
	for i, j := 0, len(v.Checkpoints)-1; i < j; i, j = i+1, j-1 {
		v.Checkpoints[i], v.Checkpoints[j] = v.Checkpoints[j], v.Checkpoints[i]
	}
	if len(v.Checkpoints) > 12 {
		v.Checkpoints = v.Checkpoints[:12]
	}
	if _, err := os.Stat(filepath.Join(s.exportDir, "receipts.jsonl")); err == nil {
		v.Exported = true
	}
	s.mu.Lock()
	v.Checks, v.CheckedAt = s.lastAudit, s.lastAuditAt
	s.mu.Unlock()
	s.render(w, r, "audit", "audit", "Audit", v)
}

func (s *Server) auditExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "auditor", "hradmin", "hrops"); !ok {
		return
	}
	s.app.Chain().RequestCheckpoint()
	if err := s.doExport(); err != nil {
		s.setFlash(r, "export failed: "+err.Error())
	} else {
		s.setFlash(r, "Bundle exported to "+s.exportDir+" — verify it offline with: go run ./cmd/verify-audit -dir export")
	}
	http.Redirect(w, r, "/audit", http.StatusSeeOther)
}

func (s *Server) doExport() error {
	if err := os.MkdirAll(s.exportDir, 0o755); err != nil {
		return err
	}
	rPath, cpPath := s.app.Chain().Paths()
	for _, pair := range [][2]string{
		{rPath, filepath.Join(s.exportDir, "receipts.jsonl")},
		{cpPath, filepath.Join(s.exportDir, "checkpoints.jsonl")},
	} {
		b, err := os.ReadFile(pair[0])
		if err != nil {
			return err
		}
		if err := os.WriteFile(pair[1], b, 0o644); err != nil {
			return err
		}
	}
	// The signer's public key is written beside the bundle from the live
	// signer (a channel outside the bundle), so the verifier can pin it.
	if pub := s.fetchSignerPub(); pub != "" {
		if err := os.WriteFile(filepath.Join(s.exportDir, "signer_pub.txt"), []byte(pub+"\n"), 0o644); err != nil {
			return err
		}
	}
	readme := "Counterfoil audit bundle.\n" +
		"Verify offline (no network, no database):\n  go run ./cmd/verify-audit -dir export\n" +
		"signer_pub.txt pins the signer's public key (fetched from the signer, not from the bundle);\n" +
		"hand it to the auditor separately, or pass it with -pub. Receipts hold hashes, not personal text.\n"
	return os.WriteFile(filepath.Join(s.exportDir, "README.txt"), []byte(readme), 0o644)
}

func (s *Server) fetchSignerPub() string {
	resp, err := http.Get(s.app.Config().SignerURL + "/pub")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != 200 || len(b) < 32 {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) auditVerify(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "auditor", "hradmin", "hrops"); !ok {
		return
	}
	if err := s.doExport(); err != nil {
		s.setFlash(r, "export failed: "+err.Error())
		http.Redirect(w, r, "/audit", http.StatusSeeOther)
		return
	}
	livePub := s.fetchSignerPub()
	checks, err := audit.Run(s.exportDir, livePub)
	if err != nil {
		s.setFlash(r, "verify failed: "+err.Error())
	} else {
		pass := 0
		for _, c := range checks {
			if c.Pass {
				pass++
			}
		}
		pin := "signer key pinned from the live signer"
		if livePub == "" {
			pin = "live signer unreachable, so signatures were checked UNPINNED"
		}
		s.setFlash(r, fmt.Sprintf("Offline verifier: %d of %d checks pass on the exported bundle (%s; same code as cmd/verify-audit).", pass, len(checks), pin))
	}
	s.mu.Lock()
	s.lastAudit, s.lastAuditAt = checks, time.Now()
	s.mu.Unlock()
	http.Redirect(w, r, "/audit", http.StatusSeeOther)
}

// ---------------------------------------------------------------- letter

func (s *Server) letterPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.user(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	c, ok := s.app.Case(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The letter belongs to its leaver; beyond them, only staff roles.
	staff := map[string]bool{"hrops": true, "hradmin": true, "auditor": true, "payroll": true}
	if u.EmpID != c.EmployeeID && !staff[u.Role] {
		http.Error(w, "403: this letter belongs to the employee it names", http.StatusForbidden)
		return
	}
	emp, _ := s.app.Employee(c.EmployeeID)
	var seq int64
	var hash string
	for _, rr := range s.app.Receipts(0, c.ID) {
		if rr.Action == "document.issued" {
			seq, hash = rr.Seq, rr.Hash
			break
		}
	}
	type vm struct {
		C    *core.Case
		Emp  core.Employee
		Seq  int64
		Hash string
	}
	s.render(w, r, "letter", "", "Relieving letter", vm{C: c, Emp: emp, Seq: seq, Hash: hash})
}
