package cli

import "errors"

// CapacityRefusal marks a lease-create error that is a definite capacity or
// quota refusal where no resource was created. Providers wrap matching
// classifier results with MarkCapacityRefusal after their own
// class/market/region/host fallback is exhausted. Core never classifies
// vendor HTTP status codes or message text.
type CapacityRefusal interface {
	error
	CapacityRefusal() bool
}

type capacityRefusalError struct {
	err error
}

func (e capacityRefusalError) Error() string         { return e.err.Error() }
func (e capacityRefusalError) Unwrap() error         { return e.err }
func (e capacityRefusalError) CapacityRefusal() bool { return true }

// MarkCapacityRefusal wraps err so IsCapacityRefusal reports true. Nil and
// already-marked errors are returned unchanged.
func MarkCapacityRefusal(err error) error {
	if err == nil || IsCapacityRefusal(err) {
		return err
	}
	return capacityRefusalError{err: err}
}

// IsCapacityRefusal reports whether err carries the CapacityRefusal marker.
func IsCapacityRefusal(err error) bool {
	if err == nil {
		return false
	}
	var marked CapacityRefusal
	return errors.As(err, &marked) && marked.CapacityRefusal()
}

// markCapacityRefusalIf wraps err when match reports a definite capacity
// refusal. Adapters call this at the end of their fallback chain.
func markCapacityRefusalIf(err error, match func(error) bool) error {
	if err == nil || match == nil || !match(err) {
		return err
	}
	return MarkCapacityRefusal(err)
}
