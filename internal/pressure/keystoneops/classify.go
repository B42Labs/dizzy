package keystoneops

import (
	"errors"

	"github.com/B42Labs/dizzy/internal/keystone"
)

// Classify labels a pressure operation error for the engine's per-bucket
// error breakdown. It is wired into the run through pressure.Config.Classify.
// An empty token pool is "pool-empty", so a run that lost its pool reads as
// such rather than as a Keystone failure; every other error gets the label
// keystone.ClassifyOperationError gives it, the classifier the churn graph
// shares, so one Keystone failure reads the same in a churn and a pressure
// report. A nil error is "".
func Classify(err error) string {
	if errors.Is(err, ErrPoolEmpty) {
		return "pool-empty"
	}
	return keystone.ClassifyOperationError(err)
}
