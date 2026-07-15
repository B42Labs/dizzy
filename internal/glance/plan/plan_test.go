package plan

import "testing"

// wellFormed is a small valid plan the tests mutate into malformed variants.
func wellFormed() *Plan {
	return &Plan{
		Scenario: "t",
		Seed:     1,
		Images: []Image{
			{Name: "img-0001", SizeMiB: 4, MetadataUpdate: true},
			{Name: "img-0002", SizeMiB: 8, Shared: true, MemberAccept: true, MemberRemove: true, Community: true, Deactivate: true, Delete: true},
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Plan)
		wantErr bool
	}{
		{name: "valid", mutate: func(*Plan) {}},
		{name: "empty name", mutate: func(p *Plan) { p.Images[0].Name = "" }, wantErr: true},
		{name: "duplicate name", mutate: func(p *Plan) { p.Images[1].Name = "img-0001" }, wantErr: true},
		{name: "zero size", mutate: func(p *Plan) { p.Images[0].SizeMiB = 0 }, wantErr: true},
		{name: "negative size", mutate: func(p *Plan) { p.Images[0].SizeMiB = -1 }, wantErr: true},
		{name: "member accept without shared", mutate: func(p *Plan) { p.Images[0].MemberAccept = true }, wantErr: true},
		{name: "member remove without shared", mutate: func(p *Plan) { p.Images[0].MemberRemove = true }, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := wellFormed()
			tc.mutate(p)
			err := p.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestCountHelpers(t *testing.T) {
	p := wellFormed()
	if got := p.MetadataUpdates(); got != 1 {
		t.Errorf("MetadataUpdates() = %d, want 1", got)
	}
	if got := p.SharedCount(); got != 1 {
		t.Errorf("SharedCount() = %d, want 1", got)
	}
	if got := p.MemberAccepts(); got != 1 {
		t.Errorf("MemberAccepts() = %d, want 1", got)
	}
	if got := p.MemberRemoves(); got != 1 {
		t.Errorf("MemberRemoves() = %d, want 1", got)
	}
	if got := p.CommunityFlips(); got != 1 {
		t.Errorf("CommunityFlips() = %d, want 1", got)
	}
	if got := p.PublicFlips(); got != 0 {
		t.Errorf("PublicFlips() = %d, want 0", got)
	}
	if got := p.Deactivates(); got != 1 {
		t.Errorf("Deactivates() = %d, want 1", got)
	}
	if got := p.Deletes(); got != 1 {
		t.Errorf("Deletes() = %d, want 1", got)
	}
	if got := p.TotalUploadMiB(); got != 12 {
		t.Errorf("TotalUploadMiB() = %d, want 12", got)
	}
}

// TestSummaryIsDeterministic asserts Summary renders the same bytes twice for
// one plan, the property the dry-run preview relies on.
func TestSummaryIsDeterministic(t *testing.T) {
	p := wellFormed()
	if first, second := p.Summary(), p.Summary(); first != second {
		t.Errorf("Summary() not deterministic:\n%s\n---\n%s", first, second)
	}
}
