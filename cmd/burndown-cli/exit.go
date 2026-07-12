package main

// exitError carries a specific process exit code up to main without printing an
// extra error line. Commands like `doctor` use it to map an aggregate status to
// a non-1 exit code after they have already written their own output.
type exitError struct {
	code int
}

func newExitError(code int) *exitError {
	return &exitError{code: code}
}

// Error implements error with an empty message so main prints nothing extra.
func (e *exitError) Error() string { return "" }

// Code returns the desired process exit code.
func (e *exitError) Code() int { return e.code }
