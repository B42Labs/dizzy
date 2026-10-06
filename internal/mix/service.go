package mix

import "fmt"

// CheckServices returns an error naming the first entry of names that this
// build does not support as an opt-in service. This build supports none, so it
// accepts only an empty list.
func CheckServices(names []string) error {
	if len(names) > 0 {
		return fmt.Errorf("opt-in service %q is not supported by this build of dizzy (supported: none)", names[0])
	}
	return nil
}
