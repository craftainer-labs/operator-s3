package s3client

// The functions below let another backend client of this operator (the AWS
// store in package awss3) report its failures with the categories, the texts
// and the log hygiene of this package, so a condition and an Event read the
// same whichever backend failed.

// Unreachable wraps the cause of a failed request (one that never got an
// answer) as lifecycle.ErrUnreachable with its category as the text, as every
// failure of a Client is: the cause stays reachable through errors.Is and
// errors.As and is never shown.
func Unreachable(cause error) error { return &connError{category: classify(cause), cause: cause} }

// AuthFailed wraps the refusal of the credentials (a 401 or 403 answer) as
// lifecycle.ErrUnreachable with the category ErrConnAuth.
func AuthFailed(cause error) error { return &connError{category: ErrConnAuth, cause: cause} }

// LoggableCode returns an S3 error code that is safe to log, or a fixed marker
// for one that is not (see loggable).
func LoggableCode(code string) string { return loggable(code) }
