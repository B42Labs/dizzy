package horizon

import (
	"errors"
	"strings"
	"testing"
)

func TestParseLoginForm(t *testing.T) {
	for _, tc := range []struct {
		name, body, token, region string
		mismatch                  bool
	}{
		{"hidden region", hiddenRegionForm, fakeCSRF, fakeRegion, false},
		{"selected option", selectRegionForm, fakeCSRF, "https://two.example/v3", false},
		{"first option", `<input name="csrfmiddlewaretoken" value="t"><select name="region"><option value="a">a</option><option value="b">b</option></select>`, "t", "a", false},
		{"self-closing inputs", `<input name="csrfmiddlewaretoken" value="t"/><input name="region" value="r"/>`, "t", "r", false},
		{"no token", `<input name="region" value="r">`, "", "", true},
		{"empty token", `<input name="csrfmiddlewaretoken" value=""><input name="region" value="r">`, "", "", true},
		{"no region", `<input name="csrfmiddlewaretoken" value="t">`, "", "", true},
		{"select without options", `<input name="csrfmiddlewaretoken" value="t"><select name="region"></select>`, "", "", true},
		{"empty body", ``, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, region, err := parseLoginForm(strings.NewReader(tc.body))
			if tc.mismatch {
				if !errors.Is(err, ErrMismatch) {
					t.Fatalf("parseLoginForm() = %q, %q, %v, want ErrMismatch", token, region, err)
				}
				return
			}
			if err != nil || token != tc.token || region != tc.region {
				t.Errorf("parseLoginForm() = %q, %q, %v, want %q, %q", token, region, err, tc.token, tc.region)
			}
		})
	}
}
