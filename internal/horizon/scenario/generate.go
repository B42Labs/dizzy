package scenario

import (
	"fmt"
	"math/rand"
	"slices"

	"github.com/B42Labs/dizzy/internal/horizon/plan"
)

// switchDrawRange bounds a session's switch draw; the target project is the
// draw modulo the number of projects the run switches between.
const switchDrawRange = 65536

// Generate expands the scenario and its seed into a fully-enumerated plan.
// The same scenario and seed always produce a byte-identical plan: the
// generator draws from math/rand v1 (whose sequence is frozen for
// compatibility) in a fixed order and emits every collection in a fixed
// order. The returned plan is validated before it is handed back.
//
// The projects project-%04d and users user-%04d draw nothing, and both
// slices are empty, but non-nil, without users, so their JSON reads [].
// Then each session session-%04d draws, in session order: its page count k
// in pages_per_session, a permutation of the pages whose first k indices,
// sorted ascending, name the pages it loads (so pages keep the scenario's
// order), whether it switches project, and its switch draw. Every draw is
// made whatever the earlier ones gave, so changing a ratio changes only the
// values it governs. Session i (from 1) logs in as user (i-1) mod users.
func (s Scenario) Generate() (*plan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	rng := rand.New(rand.NewSource(s.Seed))
	p := &plan.Plan{
		Scenario: s.Name,
		Seed:     s.Seed,
		Projects: []plan.Project{},
		Users:    []plan.User{},
		Pages:    make([]plan.Page, 0, len(s.Pages)),
		Sessions: make([]plan.Session, 0, s.Resources.Sessions),
	}

	if s.Resources.Users > 0 {
		p.Role = s.Role
		p.Projects = make([]plan.Project, 0, s.Resources.Projects)
		for i := 0; i < s.Resources.Projects; i++ {
			p.Projects = append(p.Projects, plan.Project{Name: fmt.Sprintf("project-%04d", i+1)})
		}
		p.Users = make([]plan.User, 0, s.Resources.Users)
		for i := 0; i < s.Resources.Users; i++ {
			p.Users = append(p.Users, plan.User{Name: fmt.Sprintf("user-%04d", i+1)})
		}
	}

	for _, pg := range s.Pages {
		p.Pages = append(p.Pages, plan.Page{Name: pg.Name, Path: pg.Path})
	}

	pps := s.Distribution.PagesPerSession
	for i := 0; i < s.Resources.Sessions; i++ {
		k := pps.Min + rng.Intn(pps.Max-pps.Min+1)
		// Validate caps pages_per_session.max at the page count, so the
		// permutation always holds at least k indices.
		idx := rng.Perm(len(p.Pages))[:k]
		slices.Sort(idx)
		sess := plan.Session{
			Name:  fmt.Sprintf("session-%04d", i+1),
			Pages: make([]string, 0, k),
		}
		for _, j := range idx {
			sess.Pages = append(sess.Pages, p.Pages[j].Name)
		}
		sess.Switch = rng.Float64() < s.Distribution.SwitchRatio
		sess.SwitchDraw = rng.Intn(switchDrawRange)
		if len(p.Users) > 0 {
			sess.User = p.Users[i%len(p.Users)].Name
		}
		p.Sessions = append(p.Sessions, sess)
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}
