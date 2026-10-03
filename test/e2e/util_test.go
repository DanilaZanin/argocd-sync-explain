//go:build e2e

package e2e

import "crypto/tls"

func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} } //nolint:gosec // throwaway local test cluster
