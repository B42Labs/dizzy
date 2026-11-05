package plan

import (
	"encoding/json"
	"strings"
	"testing"
)

// wellFormed is a small valid plan the tests mutate into malformed variants.
func wellFormed() *Plan {
	return &Plan{
		Scenario: "t",
		Seed:     1,
		Secrets: []Secret{
			{Name: "sec-0001", PayloadBytes: 64, Retrieve: true},
			{Name: "sec-0002", PayloadBytes: 100},
			{Name: "sec-0003", PayloadBytes: 8192, Retrieve: true},
		},
		Orders: []Order{
			{Name: "ord-0001"},
			{Name: "ord-0002"},
		},
		Containers: []Container{
			{Name: "ctr-0001", Secrets: []string{"sec-0001", "sec-0003"}},
			{Name: "ctr-0002", Secrets: []string{"sec-0002"}},
		},
	}
}

// TestValidate asserts Validate accepts the well-formed plan and rejects each
// malformed variant with an error naming the offender.
func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Plan)
		// wantErr is a substring of the expected error; empty means valid.
		wantErr string
	}{
		{name: "valid", mutate: func(*Plan) {}},
		{name: "container without references", mutate: func(p *Plan) { p.Containers[1].Secrets = []string{} }},
		{name: "empty secret name", mutate: func(p *Plan) { p.Secrets[1].Name = "" }, wantErr: "secret has an empty name"},
		{name: "empty order name", mutate: func(p *Plan) { p.Orders[1].Name = "" }, wantErr: "order has an empty name"},
		{name: "empty container name", mutate: func(p *Plan) { p.Containers[1].Name = "" }, wantErr: "container has an empty name"},
		{name: "duplicate secret name", mutate: func(p *Plan) { p.Secrets[1].Name = "sec-0001" }, wantErr: `duplicate secret name "sec-0001"`},
		{name: "duplicate order name", mutate: func(p *Plan) { p.Orders[1].Name = "ord-0001" }, wantErr: `duplicate order name "ord-0001"`},
		{name: "duplicate container name", mutate: func(p *Plan) { p.Containers[1].Name = "ctr-0001" }, wantErr: `duplicate container name "ctr-0001"`},
		{name: "zero payload", mutate: func(p *Plan) { p.Secrets[1].PayloadBytes = 0 }, wantErr: `secret "sec-0002" has payload size 0 bytes, want at least 1`},
		{name: "negative payload", mutate: func(p *Plan) { p.Secrets[1].PayloadBytes = -1 }, wantErr: `secret "sec-0002" has payload size -1 bytes, want at least 1`},
		{name: "unknown reference", mutate: func(p *Plan) { p.Containers[1].Secrets = []string{"sec-0099"} }, wantErr: `container "ctr-0002" references unknown secret "sec-0099"`},
		{name: "reference to an order", mutate: func(p *Plan) { p.Containers[1].Secrets = []string{"ord-0001"} }, wantErr: `container "ctr-0002" references unknown secret "ord-0001"`},
		{name: "empty reference", mutate: func(p *Plan) { p.Containers[1].Secrets = []string{"sec-0002", ""} }, wantErr: `container "ctr-0002" has an empty secret reference`},
		{name: "repeated reference", mutate: func(p *Plan) { p.Containers[0].Secrets = []string{"sec-0001", "sec-0003", "sec-0001"} }, wantErr: `container "ctr-0001" references secret "sec-0001" more than once`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := wellFormed()
			tc.mutate(p)
			err := p.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateAcceptsEmptyCollections asserts a plan with no secrets, orders,
// or containers is valid, whether the slices are nil or empty.
func TestValidateAcceptsEmptyCollections(t *testing.T) {
	for name, p := range map[string]*Plan{
		"nil":   {Scenario: "t", Seed: 1},
		"empty": {Scenario: "t", Seed: 1, Secrets: []Secret{}, Orders: []Order{}, Containers: []Container{}},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s collections: Validate() = %v, want nil", name, err)
		}
	}
}

func TestCountHelpers(t *testing.T) {
	p := wellFormed()
	if got := p.TotalPayloadBytes(); got != 8356 {
		t.Errorf("TotalPayloadBytes() = %d, want 8356", got)
	}
	if got := p.Retrieves(); got != 2 {
		t.Errorf("Retrieves() = %d, want 2", got)
	}
	if got := p.ContainerRefs(); got != 3 {
		t.Errorf("ContainerRefs() = %d, want 3", got)
	}

	empty := &Plan{}
	if got := empty.TotalPayloadBytes(); got != 0 {
		t.Errorf("empty TotalPayloadBytes() = %d, want 0", got)
	}
	if got := empty.Retrieves(); got != 0 {
		t.Errorf("empty Retrieves() = %d, want 0", got)
	}
	if got := empty.ContainerRefs(); got != 0 {
		t.Errorf("empty ContainerRefs() = %d, want 0", got)
	}
}

// TestSummary asserts Summary renders the header and the five count lines with
// their values aligned, and renders the same bytes twice for one plan, the
// property the dry-run preview relies on.
func TestSummary(t *testing.T) {
	p := wellFormed()
	want := `Plan for scenario "t" (seed 1)
  secrets:         3
  payload total:   8356 bytes
  read-backs:      2
  orders:          2
  containers:      2 (3 secret references)
`
	first, second := p.Summary(), p.Summary()
	if first != want {
		t.Errorf("Summary() =\n%s\nwant\n%s", first, want)
	}
	if first != second {
		t.Errorf("Summary() not deterministic:\n%s\n---\n%s", first, second)
	}
}

// TestJSONEncoding pins the JSON field names the plan file and the golden plan
// use, and that a secret not read back omits retrieve.
func TestJSONEncoding(t *testing.T) {
	p := &Plan{
		Scenario:   "t",
		Seed:       1,
		Secrets:    []Secret{{Name: "sec-0001", PayloadBytes: 64, Retrieve: true}, {Name: "sec-0002", PayloadBytes: 100}},
		Orders:     []Order{{Name: "ord-0001"}},
		Containers: []Container{{Name: "ctr-0001", Secrets: []string{"sec-0001"}}},
	}
	got, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	want := `{"scenario":"t","seed":1,` +
		`"secrets":[{"name":"sec-0001","payloadBytes":64,"retrieve":true},{"name":"sec-0002","payloadBytes":100}],` +
		`"orders":[{"name":"ord-0001"}],` +
		`"containers":[{"name":"ctr-0001","secrets":["sec-0001"]}]}`
	if string(got) != want {
		t.Errorf("json.Marshal(plan) =\n%s\nwant\n%s", got, want)
	}
}
