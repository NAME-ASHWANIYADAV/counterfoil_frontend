// Onboarding intake, the survey forms, global search, the mandates
// registry and the public about page. Same rule as everywhere: every
// figure is read from the chain, the config or the targets on request.
package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"counterfoil/internal/core"
	"counterfoil/internal/kernel"
)

// ---------------------------------------------------------------- onboarding intake

// Intake is hrops-only: the HR administrator who extends the offer
// (RSV-2) must be a different named person from whoever entered it —
// separation of duties by construction, and enforced again in
// DecideApproval for every case.
func (s *Server) onboardingNewPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops"); !ok {
		return
	}
	type vm struct {
		Managers []core.Employee
		Active   bool
	}
	v := vm{Managers: s.app.Employees()}
	if p, ok := s.app.PackageByName("onboarding"); ok {
		v.Active = p.Active
	}
	s.render(w, r, "onboarding_new", "people", "Start onboarding", v)
}

func (s *Server) onboardingStart(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops")
	if !ok {
		return
	}
	gross, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("monthly_gross")), 64)
	in := kernel.OfferInput{
		Name: r.FormValue("name"), Email: r.FormValue("email"), Title: r.FormValue("title"),
		ManagerID: r.FormValue("manager_id"), Location: r.FormValue("location"),
		JoiningDate: r.FormValue("joining_date"), MonthlyGross: gross,
	}
	id, err := s.app.StartOnboarding(u.ID, in)
	if err != nil {
		s.setFlash(r, err.Error())
		http.Redirect(w, r, "/onboarding/new", http.StatusSeeOther)
		return
	}
	s.setFlash(r, "Onboarding opened: the record was minted from the offer and the journey is running.")
	http.Redirect(w, r, "/cases/"+id, http.StatusSeeOther)
}

// ---------------------------------------------------------------- surveys

type surveyQ struct{ Key, Label, Kind string } // kind: text | select

func surveyQuestions(caseID string) (title string, qs []surveyQ) {
	if strings.HasPrefix(caseID, "ONB-") {
		return "Week-one check-in", []surveyQ{
			{"week_one", "How did the first week go?", "text"},
			{"day_one_ready", "Did they have what they needed on day one?", "select"},
			{"missing", "Anything still missing?", "text"},
		}
	}
	return "Exit survey", []surveyQ{
		{"worked_well", "What worked well here?", "text"},
		{"fix", "What should we fix?", "text"},
		{"recommend", "Would you recommend this as a place to work?", "select"},
	}
}

func (s *Server) taskForUser(u core.User, taskID string) (*core.Task, bool) {
	for _, t := range s.app.TasksFor(u.Role, u.ID) {
		if t.ID == taskID {
			return t, true
		}
	}
	return nil, false
}

func (s *Server) surveyPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	t, ok := s.taskForUser(u, r.PathValue("id"))
	if !ok {
		s.setFlash(r, "that survey task is not yours or is no longer open")
		http.Redirect(w, r, homeFor(u), http.StatusSeeOther)
		return
	}
	title, qs := surveyQuestions(t.CaseID)
	type vm struct {
		T     *core.Task
		Title string
		Qs    []surveyQ
		Back  string
	}
	back := "/work"
	if u.Role == "leaver" {
		back = "/chat"
	}
	s.render(w, r, "survey", "", title, vm{T: t, Title: title, Qs: qs, Back: back})
}

func (s *Server) surveySubmit(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	t, ok := s.taskForUser(u, r.PathValue("id"))
	if !ok {
		s.setFlash(r, "that survey task is not yours or is no longer open")
		http.Redirect(w, r, homeFor(u), http.StatusSeeOther)
		return
	}
	_, qs := surveyQuestions(t.CaseID)
	answers := map[string]string{}
	for _, q := range qs {
		answers[q.Key] = r.FormValue(q.Key)
	}
	if err := s.app.SubmitSurvey(t.ID, u.ID, answers); err != nil {
		s.setFlash(r, err.Error())
	} else {
		s.setFlash(r, "Survey recorded — the submission is a receipt on the case.")
	}
	back := "/work"
	if u.Role == "leaver" {
		back = "/chat"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// canSeeEmployee scopes the browse surfaces: HR back-office roles see
// the organisation; a manager sees themselves and their direct reports,
// never the wider workforce (or its salaries).
func canSeeEmployee(u core.User, e core.Employee) bool {
	if u.Role != "manager" {
		return true
	}
	return e.ID == u.EmpID || e.ManagerID == u.EmpID
}

func (s *Server) canSeeCase(u core.User, c *core.Case) bool {
	if c == nil {
		return false
	}
	if u.Role != "manager" {
		return true
	}
	if c.Sealed {
		return false
	}
	e, ok := s.app.Employee(c.EmployeeID)
	return ok && canSeeEmployee(u, e)
}

// ---------------------------------------------------------------- search

type searchVM struct {
	Q        string
	People   []core.Employee
	Cases    []*core.Case
	EmpOf    map[string]core.Employee
	Receipts []core.Receipt
	Clocks   []*core.Obligation
}

func (s *Server) searchPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "manager", "payroll", "hrops", "hradmin", "auditor")
	if !ok {
		return
	}
	q := strings.TrimSpace(r.FormValue("q"))
	v := searchVM{Q: q, EmpOf: map[string]core.Employee{}}
	if q != "" {
		lq := strings.ToLower(q)
		match := func(parts ...string) bool {
			for _, p := range parts {
				if strings.Contains(strings.ToLower(p), lq) {
					return true
				}
			}
			return false
		}
		// Every result set honours the same scope as its page: a manager
		// searches their reports' slice of the record, nothing wider.
		visibleCase := map[string]bool{}
		for _, c := range s.app.Cases() {
			visibleCase[c.ID] = s.canSeeCase(u, c)
		}
		for _, e := range s.app.Employees() {
			if len(v.People) < 10 && canSeeEmployee(u, e) && match(e.Name, e.Email, e.ID, e.Title) {
				v.People = append(v.People, e)
			}
		}
		for _, c := range s.app.Cases() {
			e, _ := s.app.Employee(c.EmployeeID)
			if len(v.Cases) < 10 && visibleCase[c.ID] && match(c.ID, e.Name, c.Package) {
				v.Cases = append(v.Cases, c)
				v.EmpOf[c.ID] = e
			}
		}
		seqQ, _ := strconv.ParseInt(q, 10, 64)
		for _, rc := range s.app.Receipts(0, "") { // newest-first
			if len(v.Receipts) >= 12 {
				break
			}
			if u.Role == "manager" && !visibleCase[rc.CaseID] {
				continue // platform receipts and other cases stay out of a manager's view
			}
			hashHit := len(q) >= 6 && (strings.HasPrefix(rc.Hash, lq) || strings.HasPrefix(rc.PrevHash, lq))
			if rc.Seq == seqQ || hashHit || match(rc.Action, rc.CaseID, rc.StepID) {
				v.Receipts = append(v.Receipts, rc)
			}
		}
		for _, ob := range s.app.ObligationsFor("") {
			if len(v.Clocks) < 10 && visibleCase[ob.CaseID] && match(ob.Title, ob.Basis, ob.CaseID) {
				v.Clocks = append(v.Clocks, ob)
			}
		}
	}
	s.render(w, r, "search", "", "Search", v)
}

// ---------------------------------------------------------------- mandates registry

type mandateRow struct {
	M        core.Mandate
	Approver string
	Seq      int64
	Used     bool
	Expired  bool
}

func (s *Server) mandatesPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "payroll", "hrops", "hradmin", "auditor"); !ok {
		return
	}
	used := map[string]bool{}
	var rows []mandateRow
	for _, rc := range s.app.Receipts(0, "") {
		switch rc.Action {
		case "mandate.issued":
			if m, ok2 := decodeMandate(rc.Data["mandate"]); ok2 {
				rows = append(rows, mandateRow{M: m, Approver: rc.ActorID, Seq: rc.Seq})
			}
		case "mandate.used":
			if id, ok2 := rc.Data["mandate_id"].(string); ok2 {
				used[id] = true
			}
		case "effect.dispatched", "separation.accepted", "payment.released":
			if id, ok2 := rc.Data["mandate_id"].(string); ok2 && id != "" {
				used[id] = true
			}
		}
	}
	now := time.Now().UTC()
	for i := range rows {
		rows[i].Used = used[rows[i].M.ID]
		rows[i].Expired = !rows[i].Used && now.After(rows[i].M.ExpiresAt)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq > rows[j].Seq })
	type vm struct {
		Rows   []mandateRow
		NameOf func(string) string
	}
	s.render(w, r, "mandates", "mandates", "Mandates", vm{Rows: rows, NameOf: s.userDisplay})
}

func (s *Server) userDisplay(id string) string {
	if u, ok := s.app.User(id); ok {
		return u.Name
	}
	return id
}

func decodeMandate(v any) (core.Mandate, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return core.Mandate{}, false
	}
	var m core.Mandate
	if json.Unmarshal(b, &m) != nil || m.ID == "" {
		return core.Mandate{}, false
	}
	return m, true
}

// ---------------------------------------------------------------- public about page

type aboutVM struct {
	Seq          int64
	Head         string
	Canaries     int
	CanariesHeld int
	Auto         string
	ClocksClosed int
	ClocksOnTime int
	Cases        int
}

// The footer pages are public like /about: they describe the product
// truthfully and carry no instance data that needs a session.
func (s *Server) securityPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "security", "", "Security", nil)
}

func (s *Server) privacyPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "privacy", "", "Privacy", nil)
}

func (s *Server) creditsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "credits", "", "Credits", nil)
}

func (s *Server) aboutPage(w http.ResponseWriter, r *http.Request) {
	// Public by design: the page states what the product is and shows a
	// handful of LIVE figures from this instance — not marketing numbers.
	v := aboutVM{}
	v.Seq, v.Head = s.app.Chain().Head()
	for _, c := range s.app.CanaryResults() {
		v.Canaries++
		if held, _ := c["held"].(bool); held {
			v.CanariesHeld++
		}
	}
	allDone, humanDone := 0, 0
	for _, rr := range s.app.Receipts(0, "") {
		if rr.Action == "step.status" {
			if st, _ := rr.Data["status"].(string); st == "done" || st == "skipped" {
				allDone++
				if ht, _ := rr.Data["human_touch"].(bool); ht {
					humanDone++
				}
			}
		}
	}
	if allDone > 0 {
		v.Auto = fmt.Sprintf("%d of %d", allDone-humanDone, allDone)
	}
	v.Cases = len(s.app.Cases())
	for _, ob := range s.app.ObligationsFor("") {
		if ob.Status == core.ClockClosed {
			v.ClocksClosed++
			if !ob.ClosedAt.IsZero() && ob.ClosedAt.Format("2006-01-02") <= ob.DueDate {
				v.ClocksOnTime++
			}
		}
	}
	s.render(w, r, "about", "", "About", v)
}
