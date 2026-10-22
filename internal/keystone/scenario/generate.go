package scenario

import (
	"fmt"
	"math/rand"

	"github.com/B42Labs/dizzy/internal/keystone/plan"
)

// Generate expands the scenario and its seed into a fully-enumerated plan. The
// same scenario and seed always produce a byte-identical plan: the generator
// draws from math/rand v1 (whose sequence is frozen for compatibility) in a
// fixed order and emits every collection in a fixed order. The returned plan is
// validated before it is handed back.
//
// The draw order is fixed and pinned by the golden test:
//
//  1. domains (dom-%04d) and 2. roles (role-%04d) and their implied-role rules,
//     one (prior, implied) pair per implied role in declaration order — no
//     draws; they are named scaffolding the binder maps onto created-or-reused
//     reality per tier, so a plan without rules draws exactly as before.
//  3. projects (proj-%04d) — the fixed projects count is dealt across domains in
//     round-robin batches: cycling the domain list, each turn draws a run length
//     from projects_per_domain and assigns that many next projects to the
//     current domain, until all are placed. projects_per_domain is the
//     clustering granularity, not a hard cap.
//  4. users (user-%04d) — each draws its domain uniformly.
//  5. assignments — per user in order, draw n from assignments_per_user, then
//     per draw: domain-scoped with probability domain_scoped_assignment_ratio
//     (the Float64 is drawn only when the ratio is > 0, mirroring the Cinder
//     resize guard), else project-scoped over a uniform draw of the user's
//     domain's projects (a domain with no projects falls back to a domain-scoped
//     grant), then the role uniformly. An exact-duplicate (user, target, role)
//     triple is skipped, not redrawn.
//  6. tokens — per user in order (only when users_issuing_tokens_ratio > 0),
//     draw Float64 < ratio, then scope to a uniform draw over that user's
//     distinct project-scoped grant targets (users with none are skipped).
//  7. fan users (fan-%04d) — only when fan_users > 0, so a plan without them
//     draws nothing more: per fan user in order, draw n from fan_assignments,
//     then n distinct projects of any domain by a partial Fisher–Yates shuffle
//     of the project list (the k-th draw is uniform over the projects not
//     drawn yet), each project draw followed by a uniform role draw. The
//     shuffle is undone after each fan user, so every fan user draws from the
//     plan order. Fan users live in the first domain and issue no plan token:
//     they are appended after the token draws.
func (s Scenario) Generate() (*plan.Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario: %w", err)
	}

	rng := rand.New(rand.NewSource(s.Seed))
	p := &plan.Plan{Scenario: s.Name, Seed: s.Seed}

	p.Domains = make([]plan.Domain, 0, s.Resources.Domains)
	for i := 0; i < s.Resources.Domains; i++ {
		p.Domains = append(p.Domains, plan.Domain{Name: fmt.Sprintf("dom-%04d", i+1)})
	}

	p.Roles = make([]plan.Role, 0, s.Resources.Roles)
	for i := 0; i < s.Resources.Roles; i++ {
		p.Roles = append(p.Roles, plan.Role{Name: roleName(i + 1)})
	}

	// The implied-role rules are declared, not drawn, so they leave the RNG
	// sequence undisturbed; without rules p.ImpliedRoles stays nil.
	for _, r := range s.Resources.ImpliedRoles {
		for _, implied := range r.Implies {
			p.ImpliedRoles = append(p.ImpliedRoles, plan.ImpliedRole{Prior: r.Prior, Implied: implied})
		}
	}

	// Deal the fixed project count across the domains in round-robin batches, so
	// the total is exact while projects_per_domain shapes the clustering. Each
	// turn draws a run length (>= 1, per Validate) and assigns that many next
	// projects to the current domain, then advances to the next domain.
	p.Projects = make([]plan.Project, 0, s.Resources.Projects)
	if s.Resources.Projects > 0 {
		di := 0
		for len(p.Projects) < s.Resources.Projects {
			runLen := randRange(rng, s.Distribution.ProjectsPerDomain)
			for j := 0; j < runLen && len(p.Projects) < s.Resources.Projects; j++ {
				p.Projects = append(p.Projects, plan.Project{
					Name:   fmt.Sprintf("proj-%04d", len(p.Projects)+1),
					Domain: p.Domains[di].Name,
				})
			}
			di = (di + 1) % len(p.Domains)
		}
	}

	// Each user draws its domain uniformly.
	p.Users = make([]plan.User, 0, s.Resources.Users+s.Distribution.FanUsers)
	for i := 0; i < s.Resources.Users; i++ {
		domain := p.Domains[rng.Intn(len(p.Domains))].Name
		p.Users = append(p.Users, plan.User{Name: fmt.Sprintf("user-%04d", i+1), Domain: domain})
	}

	// Group project names by domain so a user's project-scoped grants stay inside
	// its own domain (a user can only be granted a role on a project in scope).
	projectsByDomain := make(map[string][]string, len(p.Domains))
	for _, pr := range p.Projects {
		projectsByDomain[pr.Domain] = append(projectsByDomain[pr.Domain], pr.Name)
	}

	dsRatio := s.Distribution.DomainScopedAssignmentRatio
	for _, u := range p.Users {
		n := randRange(rng, s.Distribution.AssignmentsPerUser)
		domainProjects := projectsByDomain[u.Domain]
		seen := make(map[string]bool, n)
		for k := 0; k < n; k++ {
			// Draw the domain-scoped decision only when the ratio is set, so a
			// ratio-0 plan leaves the RNG sequence undisturbed.
			domainScoped := false
			if dsRatio > 0 {
				domainScoped = rng.Float64() < dsRatio
			}
			project := ""
			if !domainScoped && len(domainProjects) > 0 {
				project = domainProjects[rng.Intn(len(domainProjects))]
			}
			role := p.Roles[rng.Intn(len(p.Roles))].Name
			key := u.Name + "\x00" + project + "\x00" + role
			if seen[key] {
				continue // an exact-duplicate triple is skipped, not redrawn
			}
			seen[key] = true
			p.Assignments = append(p.Assignments, plan.Assignment{User: u.Name, Role: role, Project: project})
		}
	}

	// A user's distinct project-scoped grant targets, in first-appearance order,
	// are the scopes a token issue may draw from.
	tokenTargets := make(map[string][]string, len(p.Users))
	seenTarget := make(map[string]map[string]bool, len(p.Users))
	for _, a := range p.Assignments {
		if a.Project == "" {
			continue
		}
		if seenTarget[a.User] == nil {
			seenTarget[a.User] = make(map[string]bool)
		}
		if !seenTarget[a.User][a.Project] {
			seenTarget[a.User][a.Project] = true
			tokenTargets[a.User] = append(tokenTargets[a.User], a.Project)
		}
	}

	// Token issues are drawn only when the ratio is set, keeping a ratio-0 plan
	// byte-identical to one generated before token issues existed.
	if s.Distribution.UsersIssuingTokensRatio > 0 {
		for _, u := range p.Users {
			if rng.Float64() >= s.Distribution.UsersIssuingTokensRatio {
				continue
			}
			targets := tokenTargets[u.Name]
			if len(targets) == 0 {
				continue // no project grant to scope a token to
			}
			project := targets[rng.Intn(len(targets))]
			p.Tokens = append(p.Tokens, plan.TokenIssue{User: u.Name, Project: project})
		}
	}

	// Fan users come last and only when the scenario has any, keeping a plan
	// without them byte-identical to one generated before they existed. A fan
	// user lives in the first domain and holds grants on projects of every
	// domain, which Keystone allows; the own-domain rule of the regular grants
	// does not apply to it. Its n distinct projects come from a partial
	// Fisher–Yates shuffle of one index slice: the k-th draw swaps a uniform
	// pick among the indexes from k on into place k. Undoing the swaps in
	// reverse restores the plan order for the next fan user, so a fan user
	// costs O(n) draws and swaps rather than O(projects).
	if s.Distribution.FanUsers > 0 {
		idx := make([]int, len(p.Projects))
		for j := range idx {
			idx[j] = j
		}
		swaps := make([]int, 0, s.Distribution.FanAssignments.Max)
		for i := 0; i < s.Distribution.FanUsers; i++ {
			u := plan.User{Name: fmt.Sprintf("fan-%04d", i+1), Domain: p.Domains[0].Name, Fan: true}
			p.Users = append(p.Users, u)
			n := randRange(rng, s.Distribution.FanAssignments)
			swaps = swaps[:0]
			for k := 0; k < n; k++ {
				j := k + rng.Intn(len(idx)-k)
				idx[k], idx[j] = idx[j], idx[k]
				swaps = append(swaps, j)
				role := p.Roles[rng.Intn(len(p.Roles))].Name
				p.Assignments = append(p.Assignments, plan.Assignment{User: u.Name, Role: role, Project: p.Projects[idx[k]].Name})
			}
			for k := n - 1; k >= 0; k-- {
				j := swaps[k]
				idx[k], idx[j] = idx[j], idx[k]
			}
		}
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("generated plan failed validation: %w", err)
	}
	return p, nil
}

// randRange returns a uniformly random integer in the inclusive interval
// [r.Min, r.Max]. The caller guarantees r.Min <= r.Max via Scenario.Validate.
func randRange(rng *rand.Rand, r Range) int {
	return r.Min + rng.Intn(r.Max-r.Min+1)
}
