// Command healthcheck makes one HTTP GET and exits 0 if the status is 2xx.
//
// It exists because the runtime image is distroless: there is no shell, no curl
// and no wget, which is the property that makes the image worth using and also
// the reason a HEALTHCHECK cannot be a shell one-liner. Adding busybox to get
// one back would trade the whole benefit for a convenience.
//
// It is deliberately tiny: no flags, no redirect handling, no TLS
// configuration. A healthcheck that can fail for its own reasons is worse than
// no healthcheck.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	// All the work happens in probe so that every deferred cleanup runs before
	// os.Exit, which does not unwind the stack.
	if err := probe(); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		os.Exit(1)
	}
}

func probe() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: healthcheck <url>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, os.Args[1], http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // the process is about to exit either way
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
