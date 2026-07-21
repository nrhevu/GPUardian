package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
	"gpuardian/internal/telemetry"
)

const (
	defaultNodeResponseLimit      = 1 << 20
	defaultNodeErrorResponseLimit = 64 << 10
	maxGenericNodeJSONTokens      = 64 << 10
	maxNodeConnectionsPerHost     = 32
)

var (
	errNodeRedirect        = errors.New("node redirects are not allowed")
	defaultNodeHTTPClients = newNodeHTTPClients()
)

type NodeClient struct {
	Timeout            time.Duration
	AllowInsecureNodes bool

	clients            *nodeHTTPClients
	responseLimit      int64
	errorResponseLimit int64
}

type nodeHTTPClients struct {
	secure   *http.Client
	insecure *http.Client
}

type NodeAPI interface {
	Health(ctx context.Context, server ServerRecord, rootKey string) error
	Snapshot(ctx context.Context, server ServerRecord) (model.NodeSnapshot, error)
	CreateReservation(ctx context.Context, server ServerRecord, args protocol.RegisterArgs) (model.RegisterResult, error)
	CreateClaimKey(ctx context.Context, server ServerRecord, args protocol.RegisterArgs) (model.RegisterResult, error)
	ShowKeys(ctx context.Context, server ServerRecord, rootKey string) (model.KeyStatus, error)
	Allow(ctx context.Context, server ServerRecord, args protocol.AllowArgs) (model.AllowResult, error)
	Revoke(ctx context.Context, server ServerRecord, args protocol.RevokeArgs) (map[string]string, error)
}

type TelemetryNodeAPI interface {
	Info(ctx context.Context, server ServerRecord) (telemetry.Info, error)
	Telemetry(ctx context.Context, server ServerRecord, cursor string, limit int) (telemetry.Page, error)
}

type ManagedKeyNodeAPI interface {
	SyncManagedUserKeys(ctx context.Context, server ServerRecord, snapshot protocol.ManagedUserKeySnapshot) (protocol.ManagedUserKeySyncResult, error)
}

type TelemetryGapError struct {
	Gap telemetry.CursorGap
}

func (e *TelemetryGapError) Error() string { return e.Gap.Error() }

func (c NodeClient) Health(ctx context.Context, server ServerRecord, rootKey string) error {
	var out map[string]bool
	return c.call(ctx, server, rootKey, http.MethodGet, "/healthz", nil, &out)
}

func (c NodeClient) Snapshot(ctx context.Context, server ServerRecord) (model.NodeSnapshot, error) {
	var snapshot boundedNodeSnapshot
	err := c.call(ctx, server, server.RootKey, http.MethodGet, "/api/v1/snapshot", nil, &snapshot)
	return model.NodeSnapshot(snapshot), err
}

func (c NodeClient) Info(ctx context.Context, server ServerRecord) (telemetry.Info, error) {
	var info telemetry.Info
	err := c.call(ctx, server, server.RootKey, http.MethodGet, "/api/v1/info", nil, &info)
	return info, err
}

func (c NodeClient) Telemetry(ctx context.Context, server ServerRecord, cursor string, limit int) (telemetry.Page, error) {
	endpoint, err := joinURL(server.Endpoint, "/api/v1/telemetry", c.AllowInsecureNodes)
	if err != nil {
		return telemetry.Page{}, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return telemetry.Page{}, err
	}
	query := parsed.Query()
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", limit))
	}
	parsed.RawQuery = query.Encode()
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return telemetry.Page{}, err
	}
	req.Header.Set("Authorization", "Bearer "+server.RootKey)
	clients := c.httpClients()
	client := clients.secure
	if server.TLSSkipVerify {
		client = clients.insecure
	}
	resp, err := client.Do(req)
	if err != nil {
		return telemetry.Page{}, err
	}
	defer resp.Body.Close()
	data, err := readLimitedNodeBody(resp.Body, c.successLimit())
	if err != nil {
		return telemetry.Page{}, err
	}
	if resp.StatusCode == http.StatusGone {
		var gap telemetry.CursorGap
		if err := json.Unmarshal(data, &gap); err != nil {
			return telemetry.Page{}, err
		}
		return telemetry.Page{}, &TelemetryGapError{Gap: gap}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &body)
		if body.Error == "" {
			body.Error = resp.Status
		}
		return telemetry.Page{}, fmt.Errorf("node %s: %s", server.Name, body.Error)
	}
	if err := validateGenericNodeJSON(data); err != nil {
		return telemetry.Page{}, err
	}
	var page telemetry.Page
	if err := json.Unmarshal(data, &page); err != nil {
		return telemetry.Page{}, err
	}
	return page, nil
}

type boundedNodeSnapshot model.NodeSnapshot

func (s *boundedNodeSnapshot) UnmarshalJSON(data []byte) error {
	if err := validateNodeSnapshotJSON(data); err != nil {
		return err
	}
	type plainNodeSnapshot model.NodeSnapshot
	var snapshot plainNodeSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	*s = boundedNodeSnapshot(snapshot)
	return nil
}

func (*boundedNodeSnapshot) boundedNodeResponse() {}

type boundedKeyStatus model.KeyStatus

func (s *boundedKeyStatus) UnmarshalJSON(data []byte) error {
	if err := validateNodeKeyStatusJSON(data); err != nil {
		return err
	}
	type plainKeyStatus model.KeyStatus
	var status plainKeyStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return err
	}
	*s = boundedKeyStatus(status)
	return nil
}

func (*boundedKeyStatus) boundedNodeResponse() {}

type boundedNodeResponse interface {
	boundedNodeResponse()
}

type snapshotJSONObject int

const (
	snapshotObjectOther snapshotJSONObject = iota
	snapshotObjectTop
	snapshotObjectGPU
	snapshotObjectAuthorization
	snapshotObjectLease
	maxNodeSnapshotJSONDepth = 64
)

type snapshotJSONValidator struct {
	decoder   *json.Decoder
	kind      string
	records   int
	processes int
	depth     int
}

func validateNodeSnapshotJSON(data []byte) error {
	return validateNodeRecordJSON(data, "node snapshot")
}

func validateNodeKeyStatusJSON(data []byte) error {
	return validateNodeRecordJSON(data, "node key status")
}

func validateNodeRecordJSON(data []byte, kind string) error {
	validator := snapshotJSONValidator{decoder: json.NewDecoder(bytes.NewReader(data)), kind: kind}
	validator.decoder.UseNumber()
	token, err := validator.decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("%s must be a JSON object", kind)
	}
	if err := validator.consumeObject(snapshotObjectTop); err != nil {
		return err
	}
	if _, err := validator.decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s must contain one JSON value", kind)
		}
		return err
	}
	return nil
}

func (v *snapshotJSONValidator) consumeObject(scope snapshotJSONObject) error {
	if err := v.enterContainer(); err != nil {
		return err
	}
	defer v.leaveContainer()
	for v.decoder.More() {
		token, err := v.decoder.Token()
		if err != nil {
			return err
		}
		field, ok := token.(string)
		if !ok {
			return fmt.Errorf("%s object field must be a string", v.kind)
		}
		for i := range len(field) {
			if field[i] >= 0x80 {
				return fmt.Errorf("%s field names must be ASCII", v.kind)
			}
		}
		if err := v.consumeValue(scope, strings.ToLower(field), snapshotObjectOther); err != nil {
			return err
		}
	}
	_, err := v.decoder.Token()
	return err
}

func (v *snapshotJSONValidator) consumeValue(parent snapshotJSONObject, field string, objectScope snapshotJSONObject) error {
	token, err := v.decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return v.consumeObject(objectScope)
	case '[':
		return v.consumeArray(parent, field)
	default:
		return fmt.Errorf("unexpected JSON delimiter in %s", v.kind)
	}
}

func (v *snapshotJSONValidator) consumeArray(parent snapshotJSONObject, field string) error {
	if err := v.enterContainer(); err != nil {
		return err
	}
	defer v.leaveContainer()
	elementScope := snapshotObjectOther
	countRecords := false
	countProcesses := false
	if parent == snapshotObjectTop {
		switch field {
		case "gpus":
			countRecords = true
			elementScope = snapshotObjectGPU
		case "tokens", "reservations", "soft_claims", "bypasses", "ps":
			countRecords = true
		case "authorizations":
			countRecords = true
			elementScope = snapshotObjectAuthorization
		case "leases":
			countRecords = true
			elementScope = snapshotObjectLease
		}
	} else if parent == snapshotObjectGPU && field == "processes" {
		countRecords = true
		countProcesses = true
	} else if (parent == snapshotObjectAuthorization || parent == snapshotObjectLease) && field == "command" {
		countRecords = true
	}
	for v.decoder.More() {
		if countProcesses {
			v.processes++
			if v.processes > maxNodeSnapshotProcesses {
				return fmt.Errorf("%s exceeds %d processes", v.kind, maxNodeSnapshotProcesses)
			}
		}
		if countRecords {
			v.records++
			if v.records > maxNodeSnapshotRecords {
				return fmt.Errorf("%s exceeds %d records", v.kind, maxNodeSnapshotRecords)
			}
		}
		if err := v.consumeValue(snapshotObjectOther, "", elementScope); err != nil {
			return err
		}
	}
	_, err := v.decoder.Token()
	return err
}

func (v *snapshotJSONValidator) enterContainer() error {
	v.depth++
	if v.depth > maxNodeSnapshotJSONDepth {
		return fmt.Errorf("%s exceeds JSON nesting depth %d", v.kind, maxNodeSnapshotJSONDepth)
	}
	return nil
}

func (v *snapshotJSONValidator) leaveContainer() {
	v.depth--
}

func (c NodeClient) CreateReservation(ctx context.Context, server ServerRecord, args protocol.RegisterArgs) (model.RegisterResult, error) {
	var result model.RegisterResult
	err := c.call(ctx, server, server.RootKey, http.MethodPost, "/api/v1/reservations", args, &result)
	return result, err
}

func (c NodeClient) SyncManagedUserKeys(ctx context.Context, server ServerRecord, snapshot protocol.ManagedUserKeySnapshot) (protocol.ManagedUserKeySyncResult, error) {
	var result protocol.ManagedUserKeySyncResult
	err := c.call(ctx, server, server.RootKey, http.MethodPut, "/api/v1/user-keys/sync", snapshot, &result)
	return result, err
}

func (c NodeClient) CreateClaimKey(ctx context.Context, server ServerRecord, args protocol.RegisterArgs) (model.RegisterResult, error) {
	var result model.RegisterResult
	err := c.call(ctx, server, server.RootKey, http.MethodPost, "/api/v1/claim-keys", args, &result)
	return result, err
}

func (c NodeClient) ShowKeys(ctx context.Context, server ServerRecord, rootKey string) (model.KeyStatus, error) {
	var status boundedKeyStatus
	err := c.call(ctx, server, rootKey, http.MethodPost, "/api/v1/show-keys", nil, &status)
	return model.KeyStatus(status), err
}

func (c NodeClient) Allow(ctx context.Context, server ServerRecord, args protocol.AllowArgs) (model.AllowResult, error) {
	var result model.AllowResult
	err := c.call(ctx, server, server.RootKey, http.MethodPost, "/api/v1/allow", args, &result)
	return result, err
}

func (c NodeClient) Revoke(ctx context.Context, server ServerRecord, args protocol.RevokeArgs) (map[string]string, error) {
	var result map[string]string
	err := c.call(ctx, server, server.RootKey, http.MethodPost, "/api/v1/revoke", args, &result)
	return result, err
}

func (c NodeClient) call(ctx context.Context, server ServerRecord, rootKey, method, apiPath string, body any, out any) error {
	endpoint, err := joinURL(server.Endpoint, apiPath, c.AllowInsecureNodes)
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
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+rootKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	clients := c.httpClients()
	client := clients.secure
	if server.TLSSkipVerify {
		client = clients.insecure
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, err := readLimitedNodeBody(resp.Body, c.errorLimit())
		if err != nil {
			return fmt.Errorf("node %s: %w", server.Name, err)
		}
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &errBody)
		if errBody.Error == "" {
			errBody.Error = resp.Status
		}
		return fmt.Errorf("node %s: %s", server.Name, errBody.Error)
	}
	data, err := readLimitedNodeBody(resp.Body, c.successLimit())
	if err != nil {
		return fmt.Errorf("node %s: %w", server.Name, err)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if _, bounded := out.(boundedNodeResponse); !bounded {
		if err := validateGenericNodeJSON(data); err != nil {
			return fmt.Errorf("node %s: %w", server.Name, err)
		}
	}
	return json.Unmarshal(data, out)
}

func validateGenericNodeJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	tokens := 0
	depth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		tokens++
		if tokens > maxGenericNodeJSONTokens {
			return fmt.Errorf("node response exceeds %d JSON tokens", maxGenericNodeJSONTokens)
		}
		delim, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch delim {
		case '{', '[':
			depth++
			if depth > maxNodeSnapshotJSONDepth {
				return fmt.Errorf("node response exceeds JSON nesting depth %d", maxNodeSnapshotJSONDepth)
			}
		case '}', ']':
			depth--
		}
	}
}

func (c NodeClient) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 4 * time.Second
}

func (c NodeClient) httpClients() *nodeHTTPClients {
	if c.clients != nil {
		return c.clients
	}
	return defaultNodeHTTPClients
}

func (c NodeClient) successLimit() int64 {
	if c.responseLimit > 0 {
		return c.responseLimit
	}
	return defaultNodeResponseLimit
}

func (c NodeClient) errorLimit() int64 {
	if c.errorResponseLimit > 0 {
		return c.errorResponseLimit
	}
	return defaultNodeErrorResponseLimit
}

func newNodeHTTPClients() *nodeHTTPClients {
	secureTransport := http.DefaultTransport.(*http.Transport).Clone()
	secureTransport.Proxy = nil
	secureTransport.MaxIdleConns = maxServerRecords
	secureTransport.MaxIdleConnsPerHost = maxNodeConnectionsPerHost
	secureTransport.MaxConnsPerHost = maxNodeConnectionsPerHost
	secureTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	insecureTransport := http.DefaultTransport.(*http.Transport).Clone()
	insecureTransport.Proxy = nil
	insecureTransport.MaxIdleConns = maxServerRecords
	insecureTransport.MaxIdleConnsPerHost = maxNodeConnectionsPerHost
	insecureTransport.MaxConnsPerHost = maxNodeConnectionsPerHost
	insecureTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec
	return &nodeHTTPClients{
		secure: &http.Client{
			Transport:     secureTransport,
			CheckRedirect: rejectNodeRedirect,
		},
		insecure: &http.Client{
			Transport:     insecureTransport,
			CheckRedirect: rejectNodeRedirect,
		},
	}
}

func (c *nodeHTTPClients) closeIdleConnections() {
	c.secure.CloseIdleConnections()
	c.insecure.CloseIdleConnections()
}

func rejectNodeRedirect(_ *http.Request, _ []*http.Request) error {
	return errNodeRedirect
}

func readLimitedNodeBody(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return data, nil
}

func joinURL(base, apiPath string, allowInsecureNodes bool) (string, error) {
	parsed, err := parseNodeEndpoint(base)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" && !allowInsecureNodes {
		return "", errors.New("plaintext HTTP node endpoints are disabled; use HTTPS or set GPUARDIAN_WEB_ALLOW_INSECURE_NODES=1")
	}
	parsed.Path = path.Join(parsed.Path, apiPath)
	return parsed.String(), nil
}

func parseNodeEndpoint(base string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("node endpoint scheme must be http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("node endpoint host is required")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("node endpoint must not contain credentials, a query, or a fragment")
	}
	return parsed, nil
}
