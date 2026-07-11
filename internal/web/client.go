package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"rocguardd/internal/model"
	"rocguardd/internal/protocol"
)

type NodeClient struct {
	Timeout time.Duration
}

func (c NodeClient) Health(ctx context.Context, server ServerRecord, rootKey string) error {
	var out map[string]bool
	return c.call(ctx, server, rootKey, http.MethodGet, "/healthz", nil, &out)
}

func (c NodeClient) Snapshot(ctx context.Context, server ServerRecord) (model.NodeSnapshot, error) {
	var snapshot model.NodeSnapshot
	err := c.call(ctx, server, server.RootKey, http.MethodGet, "/api/v1/snapshot", nil, &snapshot)
	return snapshot, err
}

func (c NodeClient) CreateReservation(ctx context.Context, server ServerRecord, args protocol.RegisterArgs) (model.RegisterResult, error) {
	var result model.RegisterResult
	err := c.call(ctx, server, server.RootKey, http.MethodPost, "/api/v1/reservations", args, &result)
	return result, err
}

func (c NodeClient) CreateClaimKey(ctx context.Context, server ServerRecord, args protocol.RegisterArgs) (model.RegisterResult, error) {
	var result model.RegisterResult
	err := c.call(ctx, server, server.RootKey, http.MethodPost, "/api/v1/claim-keys", args, &result)
	return result, err
}

func (c NodeClient) ShowKeys(ctx context.Context, server ServerRecord, rootKey string) (model.KeyStatus, error) {
	var status model.KeyStatus
	err := c.call(ctx, server, rootKey, http.MethodPost, "/api/v1/show-keys", nil, &status)
	return status, err
}

func (c NodeClient) Revoke(ctx context.Context, server ServerRecord, args protocol.RevokeArgs) (map[string]string, error) {
	var result map[string]string
	err := c.call(ctx, server, server.RootKey, http.MethodPost, "/api/v1/revoke", args, &result)
	return result, err
}

func (c NodeClient) call(ctx context.Context, server ServerRecord, rootKey, method, apiPath string, body any, out any) error {
	endpoint, err := joinURL(server.Endpoint, apiPath)
	if err != nil {
		return err
	}
	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+rootKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Timeout: c.timeout(),
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: server.TLSSkipVerify}, //nolint:gosec
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		if errBody.Error == "" {
			errBody.Error = resp.Status
		}
		return fmt.Errorf("node %s: %s", server.Name, errBody.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c NodeClient) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 4 * time.Second
}

func joinURL(base, apiPath string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", err
	}
	parsed.Path = path.Join(parsed.Path, apiPath)
	return parsed.String(), nil
}
