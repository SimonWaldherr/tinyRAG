package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// basicAuthREST is the shared HTTP-GET mechanics for the REST connectors
// that authenticate via HTTP Basic: Confluence and Jira (account email +
// API token) and Freshservice (API key + the literal password "X").
// Each connector still owns its own base-URL/credential validation (see
// confGet/jiraGet/freshserviceGet) and its own query construction/JSON
// decoding — this only unifies "build the request, set the Basic-auth,
// Accept and User-Agent headers, do the round trip with a timeout and
// 429/5xx retry (see connector.go's doWithRetry/connectorHTTPClient), and
// turn a non-200 into a uniform error." label is used only in
// error-message prefixes, so existing error text (and the tests/log lines
// that depend on it) is unchanged.
type basicAuthREST struct {
	baseURL  string
	username string
	password string
	label    string
}

func (c basicAuthREST) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.baseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", connectorUserAgent)
	// false: Confluence/Jira/Freshservice are external SaaS with real,
	// publicly-trusted certificates — see doWithRetry's doc comment for why
	// insecureSkipVerify exists at all and is never appropriate here.
	raw, err := doWithRetry(req, false)
	if err != nil {
		return nil, fmt.Errorf("%s GET %s: %w", c.label, path, err)
	}
	return raw, nil
}
