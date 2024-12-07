package controller

import ctrl "sigs.k8s.io/controller-runtime"

type Result struct {
	// Result contains the result of a Reconciler invocation.
	ctrl.Result

	// Err contains an error of a Reconciler invocation
	Err error

	// StopProcessing indicates that the caller should not continue processing and
	// let the Reconciler go to sleep without an explicit requeue, expecting a
	// Watch to trigger a future reconcilation call.
	StopProcessing bool
}

func (r Result) ShouldReturn() bool {
	return r.Err != nil || !r.Result.IsZero() || r.StopProcessing
}

func (r Result) Get() (ctrl.Result, error) {
	return r.Result, r.Err
}
