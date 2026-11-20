// Package webapp is Counterfoil's product surface: chat for the leaver,
// the approval pages, the case room, the in-doubt queue, the dashboard
// computed from receipts, the admin drills and the audit room. Server-
// rendered, no JS framework, one CSS file: the design system is the
// deck's (ink, teal, amber for human-reserved moments).
package webapp

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"counterfoil/internal/audit"
	"counterfoil/internal/core"
	"counterfoil/internal/kernel"
)

//go:embed templates/*.tmpl
var tmplFS embed.FS

//go:embed static/*
var staticFS embed.FS

type Server struct {
	app  *kernel.App
	tmpl *template.Template
	mux  *http.ServeMux

	mu          sync.Mutex
	sessions    map[string]string // sid -> user id
	flash       map[string]string // sid -> one-shot message
	exportDir   string
	lastAudit   []audit.Check
	lastAuditAt time.Time
}

func New(app *kernel.App) (*Server, error) {
	funcs := template.FuncMap{
		"fdisplay":  kernel.FDisplay,
		"short":     core.Short,
		"hasPrefix": strings.HasPrefix,
		"inr": func(v any) string {
			f, ok := v.(float64)
			if !ok {
				return fmt.Sprint(v)
			}
			neg := ""
			if f < 0 {
				neg, f = "−", -f
			}
			s := fmt.Sprintf("%.2f", f)
			// Indian grouping: 12,34,567.00
			parts := strings.SplitN(s, ".", 2)
			ip := parts[0]
			if len(ip) > 3 {
				head, tail := ip[:len(ip)-3], ip[len(ip)-3:]
				var hp []string
				for len(head) > 2 {
					hp = append([]string{head[len(head)-2:]}, hp...)
					head = head[:len(head)-2]
				}
				if head != "" {
					hp = append([]string{head}, hp...)
				}
				ip = strings.Join(hp, ",") + "," + tail
			}
			return neg + "₹" + ip + "." + parts[1]
		},
		"timefmt": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2 Jan 15:04:05")
		},
		"title": func(s string) string {
			if s == "" {
				return s
			}
			return strings.ToUpper(s[:1]) + s[1:]
		},
		"lower":    strings.ToLower,
		"slicestr": func(ss ...string) []string { return ss },
	}
	t, err := template.New("").Funcs(funcs).ParseFS(tmplFS, "templates/*.tmpl")
	if err != nil {
		return nil, err
	}
	s := &Server{app: app, tmpl: t, mux: http.NewServeMux(),
		sessions: map[string]string{}, flash: map[string]string{},
		exportDir: filepath.Join(app.Config().DataDir, "..", "..", "export")}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.mux }

// ---------------------------------------------------------------- sessions

func (s *Server) sid(r *http.Request) string {
	c, err := r.Cookie("cfoil_sid")
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) user(r *http.Request) (core.User, bool) {
	s.mu.Lock()
	uid, ok := s.sessions[s.sid(r)]
	s.mu.Unlock()
	if !ok {
		return core.User{}, false
	}
	return s.app.User(uid)
}

func (s *Server) setFlash(r *http.Request, msg string) {
	s.mu.Lock()
	s.flash[s.sid(r)] = msg
	s.mu.Unlock()
}

func (s *Server) takeFlash(r *http.Request) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.flash[s.sid(r)]
	delete(s.flash, s.sid(r))
	return m
}

// ---------------------------------------------------------------- rendering

type Page struct {
	Title   string
	Active  string
	User    core.User
	Flash   string
	Kill    kernel.KillSwitch
	Partial bool
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, active, titleStr string, data any) {
	u, _ := s.user(r)
	p := Page{Title: titleStr, Active: active, User: u, Flash: s.takeFlash(r),
		Kill: s.app.Kill(), Partial: r.URL.Query().Get("partial") == "1", Data: data}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tname := name
	if p.Partial {
		tname = name + "-main"
	}
	if err := s.tmpl.ExecuteTemplate(w, tname, p); err != nil {
		fmt.Fprintf(os.Stderr, "render %s: %v\n", tname, err)
	}
}

func (s *Server) requireUser(w http.ResponseWriter, r *http.Request, roles ...string) (core.User, bool) {
	u, ok := s.user(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return u, false
	}
	if len(roles) == 0 {
		return u, true
	}
	for _, role := range roles {
		if u.Role == role {
			return u, true
		}
	}
	http.Error(w, "403: your role ("+u.Role+") cannot open this page", http.StatusForbidden)
	return u, false
}

func homeFor(u core.User) string {
	switch u.Role {
	case "leaver":
		return "/chat"
	case "manager", "payroll":
		return "/work"
	case "auditor":
		return "/audit"
	case "hradmin":
		return "/admin"
	default:
		return "/cases"
	}
}
