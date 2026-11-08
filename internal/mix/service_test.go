package mix

import (
	"slices"
	"testing"
)

func TestCheckServices(t *testing.T) {
	for name, names := range map[string][]string{"nil": nil, "empty": {}, "octavia": {"octavia"}} {
		t.Run(name, func(t *testing.T) {
			if err := CheckServices(names); err != nil {
				t.Errorf("CheckServices(%v) = %v, want nil", names, err)
			}
		})
	}

	t.Run("unsupported", func(t *testing.T) {
		for _, tc := range []struct {
			names []string
			want  string
		}{
			{[]string{"barbican"}, `opt-in service "barbican" is not supported by this build of dizzy (supported: octavia)`},
			{[]string{"octavia", "nope"}, `opt-in service "nope" is not supported by this build of dizzy (supported: octavia)`},
		} {
			if err := CheckServices(tc.names); err == nil || err.Error() != tc.want {
				t.Errorf("CheckServices(%v) = %v, want %q", tc.names, err, tc.want)
			}
		}
	})
}

// TestSupportedServicesReturnsNewSlice confirms SupportedServices lists octavia
// and hands out a new slice on every call, so a caller that changes it cannot
// change what CheckServices accepts.
func TestSupportedServicesReturnsNewSlice(t *testing.T) {
	got := SupportedServices()
	if want := []string{"octavia"}; !slices.Equal(got, want) {
		t.Fatalf("SupportedServices() = %v, want %v", got, want)
	}
	got[0] = "barbican"
	if again := SupportedServices(); again[0] != "octavia" {
		t.Errorf("SupportedServices() after changing an earlier result = %v, want [octavia]", again)
	}
	if err := CheckServices([]string{"octavia"}); err != nil {
		t.Errorf("CheckServices([octavia]) after changing an earlier result = %v, want nil", err)
	}
}
