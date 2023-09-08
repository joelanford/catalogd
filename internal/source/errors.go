package source

// UnrecoverableError represents an error that can not be recovered
// from without user intervention. When this error is returned
// the request should not be requeued.
type UnrecoverableError struct {
	err error
}

func (u *UnrecoverableError) Error() string {
	return u.err.Error()
}

func NewUnrecoverableError(err error) *UnrecoverableError {
	return &UnrecoverableError{
		err: err,
	}
}
