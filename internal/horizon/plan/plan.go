// Package plan defines the expanded, fully-enumerated Horizon workload: the
// projects and users a run creates in Keystone, the pages a session loads,
// and the sessions themselves, each with the user it logs in as, the pages
// it loads in order and whether and where it switches project. It is the
// expected-state source of truth produced deterministically from a scenario
// plus a seed. Like the other services' plans it is pure data: every
// collection is a slice (never a map), so encoding the plan to JSON yields
// byte-identical output for the same input. No password, cookie or token is
// part of a plan.
package plan

import (
	"fmt"
	"strings"
)

// Plan is the fully-expanded expected state for one Horizon run. Scenario
// and Seed record the provenance that produced it. Role is the existing role
// every created user gets on every created project; it is empty, and
// Projects and Users are empty, when the sessions log in as the clouds.yaml
// user. Pages lists the pages a session may load, in scenario order, and
// Sessions every session of one apply, which is also the envelope of chaos.
type Plan struct {
	Scenario string    `json:"scenario"`
	Seed     int64     `json:"seed"`
	Role     string    `json:"role,omitempty"`
	Projects []Project `json:"projects"`
	Users    []User    `json:"users"`
	Pages    []Page    `json:"pages"`
	Sessions []Session `json:"sessions"`
}

// Project is one project a run creates, by its logical name.
type Project struct {
	Name string `json:"name"`
}

// User is one user a run creates, by its logical name.
type User struct {
	Name string `json:"name"`
}

// Page is one dashboard page a session loads after its login. Name is the
// page's logical name, which also names its metric kind; Path is its path
// below the dashboard's base URL.
type Page struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Session is one login of one identity. User is the logical user it logs in
// as, empty for the clouds.yaml user. Pages lists the names of the pages it
// loads, in load order. Switch says whether it switches project after the
// pages, and SwitchDraw picks the target among the run's projects: the
// target is the project at index SwitchDraw modulo the number of projects.
type Session struct {
	Name       string   `json:"name"`
	User       string   `json:"user,omitempty"`
	Pages      []string `json:"pages"`
	Switch     bool     `json:"switch"`
	SwitchDraw int      `json:"switchDraw"`
}

// Validate checks the plan for well-formedness: every project, user, page
// and session has a non-empty name unique within its slice, every page has a
// path, every page a session loads names a plan page, every session names a
// plan user exactly when the plan has users, and Role and Projects are
// non-empty exactly when Users is. It returns an error naming the first
// problem.
func (p *Plan) Validate() error {
	if err := uniqueNames("project", len(p.Projects), func(i int) string { return p.Projects[i].Name }); err != nil {
		return err
	}
	if err := uniqueNames("user", len(p.Users), func(i int) string { return p.Users[i].Name }); err != nil {
		return err
	}
	if err := uniqueNames("page", len(p.Pages), func(i int) string { return p.Pages[i].Name }); err != nil {
		return err
	}
	if err := uniqueNames("session", len(p.Sessions), func(i int) string { return p.Sessions[i].Name }); err != nil {
		return err
	}

	withUsers := len(p.Users) > 0
	if withUsers != (p.Role != "") {
		return fmt.Errorf("plan has %d user(s) and role %q: a role is set exactly when users are", len(p.Users), p.Role)
	}
	if withUsers != (len(p.Projects) > 0) {
		return fmt.Errorf("plan has %d user(s) and %d project(s): projects exist exactly when users do", len(p.Users), len(p.Projects))
	}

	pages := make(map[string]bool, len(p.Pages))
	for _, pg := range p.Pages {
		if pg.Path == "" {
			return fmt.Errorf("page %q has an empty path", pg.Name)
		}
		pages[pg.Name] = true
	}
	users := make(map[string]bool, len(p.Users))
	for _, u := range p.Users {
		users[u.Name] = true
	}
	for _, s := range p.Sessions {
		for _, name := range s.Pages {
			if !pages[name] {
				return fmt.Errorf("session %q loads unknown page %q", s.Name, name)
			}
		}
		switch {
		case withUsers && s.User == "":
			return fmt.Errorf("session %q names no user, but the plan has users", s.Name)
		case withUsers && !users[s.User]:
			return fmt.Errorf("session %q names unknown user %q", s.Name, s.User)
		case !withUsers && s.User != "":
			return fmt.Errorf("session %q names user %q, but the plan has none", s.Name, s.User)
		}
	}
	return nil
}

// uniqueNames checks that each of the n names name(i) is non-empty and
// unique, naming the slice by what in an error.
func uniqueNames(what string, n int, name func(int) string) error {
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		v := name(i)
		if v == "" {
			return fmt.Errorf("%s at index %d has an empty name", what, i)
		}
		if seen[v] {
			return fmt.Errorf("duplicate %s name %q", what, v)
		}
		seen[v] = true
	}
	return nil
}

// Switches returns the number of sessions that switch project.
func (p *Plan) Switches() int {
	var n int
	for _, s := range p.Sessions {
		if s.Switch {
			n++
		}
	}
	return n
}

// Summary returns a deterministic, human-readable account of the plan, used
// by "horizon apply --dry-run" to preview a scenario without touching a
// cloud: the scenario and seed, the identity the sessions log in as, the
// session count, one line per page with the number of sessions that load
// it, and the number of sessions that switch project.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q (seed %d)\n", p.Scenario, p.Seed)
	if len(p.Users) == 0 {
		fmt.Fprintf(&b, "  identity:        the clouds.yaml user\n")
	} else {
		fmt.Fprintf(&b, "  identity:        %d created users on %d created projects, role %s\n", len(p.Users), len(p.Projects), p.Role)
	}
	fmt.Fprintf(&b, "  sessions:        %d\n", len(p.Sessions))
	loads := make(map[string]int, len(p.Pages))
	for _, s := range p.Sessions {
		for _, name := range s.Pages {
			loads[name]++
		}
	}
	for _, pg := range p.Pages {
		fmt.Fprintf(&b, "  page %s (%s): %d session(s)\n", pg.Name, pg.Path, loads[pg.Name])
	}
	fmt.Fprintf(&b, "  switches:        %d\n", p.Switches())
	return b.String()
}
