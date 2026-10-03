package collect

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

// APIClient talks to the Argo CD API server over its REST gateway (read-only GET requests).
type APIClient struct {
	Base  string // e.g. https://localhost:8080
	Token string
	HC    *http.Client
}

// NewAPIClient builds a client. plainText selects http, insecure skips TLS verification.
func NewAPIClient(server, token string, plainText, insecure bool) *APIClient {
	scheme := "https"
	if plainText {
		scheme = "http"
	}
	base := server
	if !strings.Contains(server, "://") {
		base = scheme + "://" + server
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}} //nolint:gosec // opt-in flag, same as argocd --insecure
	return &APIClient{Base: strings.TrimRight(base, "/"), Token: token, HC: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// HTTPError is a non-2xx API answer.
type HTTPError struct {
	Status int
	Body   string
	Path   string
}

func (e *HTTPError) Error() string {
	b := strings.TrimSpace(e.Body)
	if len(b) > 200 {
		b = b[:200]
	}
	return fmt.Sprintf("HTTP %d from %s: %s", e.Status, e.Path, b)
}

func (c *APIClient) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.Base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		// Secrets are removed before the body is truncated or shown anywhere.
		return &HTTPError{Status: resp.StatusCode, Body: Redact(string(body), c.Token), Path: path}
	}
	return json.Unmarshal(body, out)
}

func unavail(err error, what string) error {
	if he, ok := err.(*HTTPError); ok {
		switch he.Status {
		case http.StatusForbidden:
			return &ErrUnavailable{Reason: fmt.Sprintf("%s: denied or does not exist (Argo CD answers HTTP 403 for both: the token's RBAC or the allowed namespaces may refuse it, or the name may be wrong)", what), Denied: true}
		case http.StatusUnauthorized:
			return &ErrUnavailable{Reason: fmt.Sprintf("%s: authentication failed (HTTP 401); the token is missing, expired or invalid", what)}
		case http.StatusNotFound:
			return &ErrUnavailable{Reason: fmt.Sprintf("%s: not found (HTTP 404)", what)}
		}
	}
	return err
}

// Mode implements Source.
func (c *APIClient) Mode() string { return "api" }

// Version implements Source.
func (c *APIClient) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"Version"`
	}
	if err := c.get(ctx, "/api/version", nil, &v); err != nil {
		return "", unavail(err, "server version")
	}
	return v.Version, nil
}

func nsQuery(ns string) url.Values {
	q := url.Values{}
	if ns != "" {
		q.Set("appNamespace", ns)
	}
	return q
}

// Application implements Source.
func (c *APIClient) Application(ctx context.Context, ns, name string) (*model.Application, error) {
	var a model.Application
	if err := c.get(ctx, "/api/v1/applications/"+url.PathEscape(name), nsQuery(ns), &a); err != nil {
		return nil, unavail(err, "application")
	}
	if a.Metadata.Namespace == "" {
		a.Metadata.Namespace = ns
	}
	return &a, nil
}

// Project implements Source.
func (c *APIClient) Project(ctx context.Context, name string) (*model.AppProject, error) {
	var p model.AppProject
	if err := c.get(ctx, "/api/v1/projects/"+url.PathEscape(name), nil, &p); err != nil {
		return nil, unavail(err, "AppProject "+name)
	}
	return &p, nil
}

// Tree implements Source.
func (c *APIClient) Tree(ctx context.Context, ns, name string) (*model.ResourceTree, error) {
	var t model.ResourceTree
	if err := c.get(ctx, "/api/v1/applications/"+url.PathEscape(name)+"/resource-tree", nsQuery(ns), &t); err != nil {
		return nil, unavail(err, "resource tree")
	}
	return &t, nil
}

// Events implements Source.
func (c *APIClient) Events(ctx context.Context, ns, name string) ([]model.Event, error) {
	var l struct {
		Items []model.Event `json:"items"`
	}
	if err := c.get(ctx, "/api/v1/applications/"+url.PathEscape(name)+"/events", nsQuery(ns), &l); err != nil {
		return nil, unavail(err, "events")
	}
	sortEvents(l.Items)
	return l.Items, nil
}

// Live implements LiveSource: the live manifest of one managed resource.
func (c *APIClient) Live(ctx context.Context, ns, name string, rs model.ResourceStatus) (*model.LiveObject, error) {
	q := nsQuery(ns)
	q.Set("namespace", rs.Namespace)
	q.Set("resourceName", rs.Name)
	q.Set("version", rs.Version)
	q.Set("group", rs.Group)
	q.Set("kind", rs.Kind)
	var out struct {
		Manifest string `json:"manifest"`
	}
	if err := c.get(ctx, "/api/v1/applications/"+url.PathEscape(name)+"/resource", q, &out); err != nil {
		if he, ok := err.(*HTTPError); ok && (he.Status == http.StatusNotFound || strings.Contains(he.Body, "not found")) {
			return &model.LiveObject{Exists: false}, nil
		}
		return nil, unavail(err, "live "+rs.Kind+"/"+rs.Name)
	}
	var obj struct {
		Metadata model.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out.Manifest), &obj); err != nil {
		return nil, fmt.Errorf("live %s/%s: %w", rs.Kind, rs.Name, err)
	}
	return &model.LiveObject{Exists: true, DeletionTimestamp: obj.Metadata.DeletionTimestamp, Finalizers: obj.Metadata.Finalizers}, nil
}
