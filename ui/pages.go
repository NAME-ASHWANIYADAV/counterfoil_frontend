// The record, browsable: single receipts, people, the clocks board, the
// settlement statement, and the doctrine page. Every figure on these
// pages is read from the chain, the live config, or the target systems
// at request time — nothing is staged.
package webapp

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"counterfoil/internal/core"
	"counterfoil/internal/kernel"
)

// ---------------------------------------------------------------- one receipt

type receiptVM struct {
	R          core.Receipt
	Recomputed string
	Match      bool
	HasPrev    bool
	HasNext    bool
	PrevSeq    int64
	NextSeq    int64
	Covered    bool
	CoverNote  string
	DataJSON   string
}

func (s *Server) receiptPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq < 1 {
		s.notFound(w, r)
		return
	}
	rc, ok := s.app.ReceiptBySeq(seq)
	if !ok {
		s.notFound(w, r)
		return
	}
	// A leaver sees their own case's counterfoils, nothing else.
	if u.Role == "leaver" {
		c, has := s.app.CaseForEmployee(u.EmpID)
		if !has || rc.CaseID != c.ID {
			http.Error(w, "403: this counterfoil is not on your case", http.StatusForbidden)
			return
		}
	}
	v := receiptVM{R: rc, Recomputed: core.ReceiptHash(rc), PrevSeq: seq - 1, NextSeq: seq + 1}
	v.Match = v.Recomputed == rc.Hash
	// The pager never points at a counterfoil the viewer cannot open: a
	// leaver walks only their own case's chain.
	if prev, ok := s.app.ReceiptBySeq(seq - 1); ok {
		v.HasPrev = u.Role != "leaver" || prev.CaseID == rc.CaseID
	}
	if next, ok := s.app.ReceiptBySeq(seq + 1); ok {
		v.HasNext = u.Role != "leaver" || next.CaseID == rc.CaseID
	}
	cp, signerErr := s.app.Chain().LastCheckpoint()
	switch {
	case signerErr != "" && cp.UptoSeq == 0:
		v.CoverNote = "countersignature status unknown — the signer is not reachable"
	case cp.UptoSeq >= seq:
		v.Covered = true
		v.CoverNote = fmt.Sprintf("countersigned: the signer's checkpoint at seq %d covers this receipt", cp.UptoSeq)
	default:
		v.CoverNote = fmt.Sprintf("chain-protected; the next checkpoint will countersign it (last checkpoint: seq %d)", cp.UptoSeq)
	}
	if len(rc.Data) > 0 {
		b, _ := json.MarshalIndent(rc.Data, "", "  ")
		v.DataJSON = string(b)
	}
	s.render(w, r, "receipt", "", fmt.Sprintf("Receipt #%d", seq), v)
}

// ---------------------------------------------------------------- people

type personRow struct {
	E           core.Employee
	ManagerName string
	Case        *core.Case
	CaseDone    int
	CaseAll     int
}

func (s *Server) peoplePage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "manager", "payroll", "hrops", "hradmin", "auditor")
	if !ok {
		return
	}
	var rows []personRow
	for _, e := range s.app.Employees() {
		if !canSeeEmployee(u, e) {
			continue
		}
		row := personRow{E: e}
		if m, ok := s.app.Employee(e.ManagerID); ok {
			row.ManagerName = m.Name
		}
		if c, ok := s.app.CaseForEmployee(e.ID); ok {
			row.Case = c
			for _, st := range c.Steps {
				row.CaseAll++
				if st.Status == core.StepDone || st.Status == core.StepSkipped {
					row.CaseDone++
				}
			}
		}
		rows = append(rows, row)
	}
	s.render(w, r, "people", "people", "People", rows)
}

type personVM struct {
	E        core.Employee
	Manager  core.Employee
	Case     *core.Case
	Clocks   []*core.Obligation
	Dir      map[string]any
	DirErr   string
	DirNone  bool
	Card     map[string]any
	CardErr  string
	CardNone bool
	Receipts []core.Receipt
}

func (s *Server) personPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "manager", "payroll", "hrops", "hradmin", "auditor")
	if !ok {
		return
	}
	e, ok := s.app.Employee(r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
		return
	}
	if !canSeeEmployee(u, e) {
		http.Error(w, "403: a manager sees their own reports, not the wider workforce", http.StatusForbidden)
		return
	}
	v := personVM{E: e}
	v.Manager, _ = s.app.Employee(e.ManagerID)
	if c, has := s.app.CaseForEmployee(e.ID); has {
		v.Case = c
		v.Clocks = s.app.ObligationsFor(c.ID)
		rs := s.app.Receipts(0, c.ID)
		sort.Slice(rs, func(i, j int) bool { return rs[i].Seq > rs[j].Seq })
		if len(rs) > 8 {
			rs = rs[:8]
		}
		v.Receipts = rs
	}
	// Live reads of the targets' own records — the same reads the
	// dispatcher's read-back performs. An absent record is a clean fact
	// (a joiner before provisioning); a transport failure is an error.
	if d, err := s.app.LiveDirectory(e.Email); err == nil {
		v.Dir = d
	} else if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "no email") {
		v.DirNone = true
	} else {
		v.DirErr = err.Error()
	}
	if c, err := s.app.LiveCard(e.CardID); err == nil {
		v.Card = c
	} else if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "no card") {
		v.CardNone = true
	} else {
		v.CardErr = err.Error()
	}
	s.render(w, r, "person", "people", e.Name, v)
}

// ---------------------------------------------------------------- clocks board

type clocksVM struct {
	Active  []*core.Obligation
	Closed  []*core.Obligation
	CaseOf  map[string]*core.Case
	EmpOf   map[string]core.Employee
	AtRisk  int
	Soonest string // "Wed 25 Nov 2026 (5d)" or ""
	OnTime  int
}

func (s *Server) clocksPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "manager", "payroll", "hrops", "hradmin", "auditor")
	if !ok {
		return
	}
	v := clocksVM{CaseOf: map[string]*core.Case{}, EmpOf: map[string]core.Employee{}}
	for _, ob := range s.app.ObligationsFor("") {
		c, hasCase := s.app.Case(ob.CaseID)
		if !hasCase || !s.canSeeCase(u, c) {
			continue
		}
		v.CaseOf[ob.CaseID] = c
		v.EmpOf[ob.CaseID], _ = s.app.Employee(c.EmployeeID)
		if ob.Status == core.ClockClosed {
			v.Closed = append(v.Closed, ob)
		} else {
			v.Active = append(v.Active, ob)
		}
	}
	sort.Slice(v.Active, func(i, j int) bool { return v.Active[i].DueDate < v.Active[j].DueDate })
	sort.Slice(v.Closed, func(i, j int) bool { return v.Closed[i].ClosedAt.After(v.Closed[j].ClosedAt) })
	for _, ob := range v.Active {
		if ob.Status == core.ClockAtRisk || ob.Status == core.ClockBreached {
			v.AtRisk++
		}
	}
	for _, ob := range v.Closed {
		if !ob.ClosedAt.IsZero() && ob.ClosedAt.Format("2006-01-02") <= ob.DueDate {
			v.OnTime++
		}
	}
	if len(v.Active) > 0 {
		v.Soonest = kernel.FDisplay(v.Active[0].DueDate)
	}
	s.render(w, r, "clocksboard", "clocks", "Clocks", v)
}

// ---------------------------------------------------------------- settlement statement

type settlementVM struct {
	C        *core.Case
	Emp      core.Employee
	FF       *kernel.FFFacts
	Released bool
	RelSeq   int64
	RelEv    string
	Mandate  string
}

func (s *Server) settlementPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.user(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	c, ok := s.app.Case(r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
		return
	}
	staff := map[string]bool{"payroll": true, "hrops": true, "hradmin": true, "auditor": true}
	if u.EmpID != c.EmployeeID && !staff[u.Role] {
		http.Error(w, "403: this statement belongs to the employee it names", http.StatusForbidden)
		return
	}
	v := settlementVM{C: c}
	v.Emp, _ = s.app.Employee(c.EmployeeID)
	if raw, ok := c.Facts["ff"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			var f kernel.FFFacts
			if json.Unmarshal(b, &f) == nil {
				v.FF = &f
			}
		}
	}
	for _, rc := range s.app.Receipts(0, c.ID) {
		if rc.Action == "payment.released" {
			v.Released, v.RelSeq, v.RelEv = true, rc.Seq, rc.Evidence
			v.Mandate, _ = rc.Data["mandate_id"].(string)
		}
	}
	s.render(w, r, "settlement", "", "Settlement statement", v)
}

// ---------------------------------------------------------------- doctrine

type doctrineVM struct {
	Reserved []kernel.ReservedDef
	Rules    *kernel.RulePack
	Holidays []string
}

func (s *Server) doctrinePage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	v := doctrineVM{Reserved: s.app.ReservedList(), Rules: s.app.Rules()}
	for d, name := range v.Rules.Holidays {
		v.Holidays = append(v.Holidays, kernel.FDisplay(d)+" — "+name)
	}
	sort.Strings(v.Holidays)
	s.render(w, r, "doctrine", "doctrine", "How Counterfoil decides", v)
}

// ---------------------------------------------------------------- 404

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	// The path is attacker-controlled input reflected into HTML: escape
	// it (Go's %q escapes string syntax, not markup) and keep it short.
	path := r.URL.Path
	if len(path) > 80 {
		path = path[:80] + "…"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>Not on the record · Counterfoil</title>
<link rel="stylesheet" href="/static/app.css"></head>
<body style="background:var(--navy);color:#fff;display:flex;min-height:100vh;align-items:center;justify-content:center;text-align:center">
<div><div class="mark wm" style="font-size:22px">COUNTER<span class="tear"></span><span class="leaf">FOIL</span></div>
<p style="color:var(--fog);max-width:40ch;margin:16px auto">There is no receipt for &ldquo;%s&rdquo; — this page is not on the record.</p>
<p><a href="/" style="color:var(--sky)">Back to the product</a></p></div></body></html>`, html.EscapeString(path))
}
