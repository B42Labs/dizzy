package keystoneops

import (
	"errors"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/B42Labs/dizzy/internal/keystone"
	"github.com/B42Labs/dizzy/internal/pressure/probe"
)

// Classify labels a pressure operation error for the engine's per-bucket
// error breakdown. It is wired into the run through pressure.Config.Classify.
// An empty token pool is "pool-empty", so a run that lost its pool reads as
// such rather than as a Keystone failure. A drawn token whose catalog lists
// none of the four services is "no-service", a service that let a bad token
// through is "accepted", and a 401 is "unauthorized", so a service refusing a
// live pool token stands out in the breakdown. Every other error gets the
// label keystone.ClassifyOperationError gives it, the classifier the churn
// graph shares, so one Keystone failure reads the same in a churn and a
// pressure report. The checks run in that order, and a nil error is "".
func Classify(err error) string {
	switch {
	case errors.Is(err, ErrPoolEmpty):
		return "pool-empty"
	case errors.Is(err, ErrNoService):
		return "no-service"
	case errors.Is(err, probe.ErrAccepted):
		return "accepted"
	case gophercloud.ResponseCodeIs(err, 401):
		return "unauthorized"
	}
	return keystone.ClassifyOperationError(err)
}
