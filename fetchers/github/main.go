// Command fetch is the JOBS GitHub fetcher: it imports a GitHub repository tree
// at a given ref (tag, branch, or SHA) into amber. It conforms to the fetcher
// execution contract (architecture/import.md §3.3): params arrive in
// JOBS_FETCH_PARAMS, an optional host-scoped token in JOBS_SECRETS_FILE, and the
// result tree is written to JOBS_OUTPUT_DIR. Exit 0 = success, 75 = retryable,
// any other non-zero = hard failure.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	exitOK        = 0
	exitHard      = 1
	exitRetryable = 75
)

// classifiedError carries whether a failure is retryable so run() can map it to
// the right exit code.
type classifiedError struct {
	retryable bool
	msg       string
}

func (e *classifiedError) Error() string { return e.msg }

func hardErr(format string, a ...any) error {
	return &classifiedError{msg: fmt.Sprintf(format, a...)}
}

func retryErr(format string, a ...any) error {
	return &classifiedError{retryable: true, msg: fmt.Sprintf(format, a...)}
}

func isRetryable(err error) bool {
	var ce *classifiedError
	if errors.As(err, &ce) {
		return ce.retryable
	}
	return false
}

func main() {
	os.Exit(run(os.Getenv, os.Stderr))
}

// run is the testable entrypoint.
func run(getenv func(string) string, stderr io.Writer) int {
	ctx := context.Background()

	outDir := getenv("JOBS_OUTPUT_DIR")
	if outDir == "" {
		fmt.Fprintln(stderr, "JOBS_OUTPUT_DIR not set")
		return exitHard
	}

	p, err := parseParams([]byte(getenv("JOBS_FETCH_PARAMS")))
	if err != nil {
		fmt.Fprintf(stderr, "params: %v\n", err)
		return exitHard
	}

	host, err := apiHost(p.APIBaseURL)
	if err != nil {
		fmt.Fprintf(stderr, "apiBaseURL: %v\n", err)
		return exitHard
	}

	var token string
	if sf := getenv("JOBS_SECRETS_FILE"); sf != "" {
		b, err := os.ReadFile(sf)
		if err != nil {
			fmt.Fprintf(stderr, "read secrets: %v\n", err)
			return exitRetryable
		}
		t, ok, err := selectToken(b, host)
		if err != nil {
			fmt.Fprintf(stderr, "secrets: %v\n", err)
			return exitHard
		}
		if ok {
			token = t
		}
	}

	body, err := fetch(ctx, p, host, token)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if isRetryable(err) {
			return exitRetryable
		}
		return exitHard
	}
	defer body.Close()

	if err := extractTarball(body, outDir); err != nil {
		fmt.Fprintln(stderr, err)
		if isRetryable(err) {
			return exitRetryable
		}
		return exitHard
	}
	return exitOK
}
