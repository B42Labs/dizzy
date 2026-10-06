package mix

import "testing"

func TestCheckServices(t *testing.T) {
	for name, names := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			if err := CheckServices(names); err != nil {
				t.Errorf("CheckServices(%v) = %v, want nil", names, err)
			}
		})
	}

	t.Run("none supported", func(t *testing.T) {
		want := `opt-in service "octavia" is not supported by this build of dizzy (supported: none)`
		if err := CheckServices([]string{"octavia"}); err == nil || err.Error() != want {
			t.Errorf("CheckServices = %v, want %q", err, want)
		}
	})
}
