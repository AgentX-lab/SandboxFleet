package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
)

type Client struct {
	httpClient *http.Client
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{httpClient: httpClient}
}

func (c *Client) Health(ctx context.Context, endpoint string) error {
	return c.do(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/healthz", nil, nil)
}

func (c *Client) ListSlots(ctx context.Context, endpoint string) ([]slot.Info, error) {
	var result []slot.Info
	err := c.do(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/slots", nil, &result)
	return result, err
}

func (c *Client) ApplyTopology(ctx context.Context, endpoint string, configs []slot.Config) error {
	if configs == nil {
		configs = []slot.Config{}
	}
	return c.do(ctx, http.MethodPut, strings.TrimRight(endpoint, "/")+"/v1/topology", configs, nil)
}

func (c *Client) ReserveSlot(ctx context.Context, endpoint string, ref worker.SandboxSlotRef) error {
	return c.do(ctx, http.MethodPost, slotURL(endpoint, ref.SlotID, "reserve"), ref, nil)
}

func (c *Client) StartSandbox(ctx context.Context, endpoint string, req worker.StartSandboxRequest) error {
	return c.do(ctx, http.MethodPost, slotURL(endpoint, req.SlotID, "start"), req, nil)
}

func (c *Client) StopSandbox(ctx context.Context, endpoint string, ref worker.SandboxSlotRef) error {
	return c.do(ctx, http.MethodPost, slotURL(endpoint, ref.SlotID, "stop"), ref, nil)
}

func (c *Client) ReleaseSlot(ctx context.Context, endpoint string, ref worker.SandboxSlotRef) error {
	return c.do(ctx, http.MethodPost, slotURL(endpoint, ref.SlotID, "release"), ref, nil)
}

func (c *Client) ExecSandbox(ctx context.Context, endpoint string, req worker.ExecSandboxRequest) (worker.ExecSandboxResult, error) {
	var result worker.ExecSandboxResult
	err := c.do(ctx, http.MethodPost, slotURL(endpoint, req.SlotID, "exec"), req, &result)
	return result, err
}

func (c *Client) ExistsSandboxFile(ctx context.Context, endpoint string, req worker.SandboxFileRequest) (bool, error) {
	var result struct {
		Exists bool `json:"exists"`
	}
	err := c.do(ctx, http.MethodGet, fileURL(endpoint, req.SlotID, "exists", req), nil, &result)
	return result.Exists, err
}

func (c *Client) ListSandboxFiles(ctx context.Context, endpoint string, req worker.SandboxFileRequest) ([]worker.SandboxFileEntry, error) {
	var result []worker.SandboxFileEntry
	err := c.do(ctx, http.MethodGet, fileURL(endpoint, req.SlotID, "list", req), nil, &result)
	if result == nil && err == nil {
		result = []worker.SandboxFileEntry{}
	}
	return result, err
}

func (c *Client) ReadSandboxFile(ctx context.Context, endpoint string, req worker.SandboxFileRequest) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL(endpoint, req.SlotID, "content", req), nil)
	if err != nil {
		return nil, fmt.Errorf("build worker request: %w", err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call worker API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var details errorResponse
		if err := json.NewDecoder(response.Body).Decode(&details); err != nil {
			details.Message = response.Status
		}
		return nil, &Error{StatusCode: response.StatusCode, Code: details.Code, Message: details.Message}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(worker.MaxFileBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read worker response: %w", err)
	}
	if len(data) > worker.MaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", worker.MaxFileBytes)
	}
	return data, nil
}

func (c *Client) WriteSandboxFile(ctx context.Context, endpoint string, req worker.SandboxFileRequest, content []byte) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", req.Path)
	if err != nil {
		return fmt.Errorf("create multipart file: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return fmt.Errorf("write multipart content: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	requestURL := fileURL(endpoint, req.SlotID, "upload", req)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, &body)
	if err != nil {
		return fmt.Errorf("build worker request: %w", err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call worker API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var details errorResponse
		if err := json.NewDecoder(response.Body).Decode(&details); err != nil {
			details.Message = response.Status
		}
		return &Error{StatusCode: response.StatusCode, Code: details.Code, Message: details.Message}
	}
	return nil
}

func (c *Client) GetSandbox(ctx context.Context, endpoint string, ref worker.SandboxSlotRef) (worker.SandboxInfo, error) {
	query := url.Values{
		"namespace": []string{ref.Identity.Namespace},
		"name":      []string{ref.Identity.Name},
		"uid":       []string{string(ref.Identity.UID)},
	}
	var result worker.SandboxInfo
	err := c.do(ctx, http.MethodGet, slotURL(endpoint, ref.SlotID, "")+"?"+query.Encode(), nil, &result)
	return result, err
}

type Error struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("worker API %s (%d): %s", e.Code, e.StatusCode, e.Message)
}

func (e *Error) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests ||
		e.StatusCode == http.StatusInternalServerError ||
		e.StatusCode == http.StatusServiceUnavailable
}

func (c *Client) do(ctx context.Context, method, requestURL string, body, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode worker request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	request, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return fmt.Errorf("build worker request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call worker API: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var details errorResponse
		if err := json.NewDecoder(response.Body).Decode(&details); err != nil {
			details.Message = response.Status
		}
		return &Error{StatusCode: response.StatusCode, Code: details.Code, Message: details.Message}
	}
	if result == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		return fmt.Errorf("decode worker response: %w", err)
	}
	return nil
}

func slotURL(endpoint string, slotID int32, action string) string {
	result := fmt.Sprintf("%s/v1/slots/%d", strings.TrimRight(endpoint, "/"), slotID)
	if action != "" {
		result += "/" + action
	}
	return result
}

func fileURL(endpoint string, slotID int32, action string, req worker.SandboxFileRequest) string {
	query := url.Values{
		"namespace": []string{req.Identity.Namespace},
		"name":      []string{req.Identity.Name},
		"uid":       []string{string(req.Identity.UID)},
	}
	if action != "upload" {
		query.Set("path", req.Path)
	}
	return fmt.Sprintf("%s/v1/slots/%d/files/%s?%s", strings.TrimRight(endpoint, "/"), slotID, action, query.Encode())
}
