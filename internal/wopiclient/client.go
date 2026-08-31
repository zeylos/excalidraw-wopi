// Package wopiclient implements a plain WOPI HTTP client: CheckFileInfo,
// GetFile, PutFile, and the lock operations. It knows nothing about a
// specific host's quirks; a Client delegates status-code interpretation and
// version-header extraction to a HostProfile, so this package speaks only
// the WOPI protocol as specified.
package wopiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultTimeout bounds a whole WOPI request, so a hung or SSRF-redirected
// host cannot tie up a goroutine indefinitely. It covers a large GetFile
// transfer plus network latency; a host slower than this is unusable in
// practice regardless.
const defaultTimeout = 60 * time.Second

// defaultHTTPClient builds the http.Client New falls back to when the
// caller passes a nil one. WOPI never needs a redirect (every operation
// targets one exact WOPISrc URL); refusing all of them closes off an SSRF
// vector where a malicious or compromised host redirects a signed request
// to an internal address.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Op names identify a WOPI operation in errors and in a HostProfile call.
const (
	OpCheckFileInfo   = "CheckFileInfo"
	OpGetFile         = "GetFile"
	OpPutFile         = "PutFile"
	OpLock            = "Lock"
	OpGetLock         = "GetLock"
	OpRefreshLock     = "RefreshLock"
	OpUnlock          = "Unlock"
	OpUnlockAndRelock = "UnlockAndRelock"
)

const (
	headerOverride        = "X-WOPI-Override"
	headerLock            = "X-WOPI-Lock"
	headerOldLock         = "X-WOPI-OldLock"
	headerProof           = "X-WOPI-Proof"
	headerProofOld        = "X-WOPI-ProofOld"
	headerTimestamp       = "X-WOPI-TimeStamp"
	headerMaxExpectedSize = "X-WOPI-MaxExpectedSize"
	headerContentType     = "Content-Type"
)

// RequestSigner signs a WOPI request. A host that publishes a proof key
// verifies the signature against the access token and the full request URL.
type RequestSigner interface {
	Sign(accessToken, url string) (proof, proofOld, timestamp string)
}

// HostProfile carries the host-specific decisions a Client defers to: how
// to read a status code, and which response header holds the version
// marker. internal/hostadapter implements it per WOPI host.
type HostProfile interface {
	// MapError maps a non-2xx status to a typed error. It returns nil when
	// the status carries no host-specific meaning, so the Client falls back
	// to a generic HTTPError.
	MapError(op string, status int, wopiLockHeader string) error
	// ItemVersion reads the version marker from a response header set.
	ItemVersion(header http.Header) string
}

// Client is a WOPI HTTP client. The zero value is not usable; build one
// with New.
type Client struct {
	http    *http.Client
	signer  RequestSigner
	profile HostProfile
}

// New builds a Client. httpClient defaults to a client with a bounded
// timeout and no redirect following (see defaultHTTPClient) when nil.
// signer may be nil to send unsigned requests, for development against a
// host that runs without proof checking.
func New(httpClient *http.Client, signer RequestSigner, profile HostProfile) *Client {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	return &Client{http: httpClient, signer: signer, profile: profile}
}

// CheckFileInfo calls the WOPI CheckFileInfo operation on src.
func (c *Client) CheckFileInfo(ctx context.Context, src, token string) (FileInfo, error) {
	reqURL, err := withAccessToken(src, token)
	if err != nil {
		return FileInfo{}, err
	}
	resp, err := c.send(ctx, OpCheckFileInfo, http.MethodGet, reqURL, token, nil, nil)
	if err != nil {
		return FileInfo{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return FileInfo{}, fmt.Errorf("wopiclient: %s: read response: %w", OpCheckFileInfo, err)
	}
	if resp.StatusCode != http.StatusOK {
		return FileInfo{}, c.mapErr(OpCheckFileInfo, resp, body)
	}

	var info FileInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return FileInfo{}, fmt.Errorf("wopiclient: %s: decode response: %w", OpCheckFileInfo, err)
	}
	return info, nil
}

// GetFile calls the WOPI GetFile operation on src. The caller must close
// the returned ReadCloser. maxExpectedSize is optional; when given, its
// first value is sent as X-WOPI-MaxExpectedSize, a hint some hosts honor
// to reject an oversize file before transferring the whole body.
func (c *Client) GetFile(ctx context.Context, src, token string, maxExpectedSize ...int64) (io.ReadCloser, string, error) {
	contents, err := contentsURL(src)
	if err != nil {
		return nil, "", err
	}
	reqURL, err := withAccessToken(contents, token)
	if err != nil {
		return nil, "", err
	}
	var header http.Header
	if len(maxExpectedSize) > 0 {
		header = http.Header{}
		header.Set(headerMaxExpectedSize, strconv.FormatInt(maxExpectedSize[0], 10))
	}
	resp, err := c.send(ctx, OpGetFile, http.MethodGet, reqURL, token, header, nil)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, "", c.mapErr(OpGetFile, resp, body)
	}
	return resp.Body, c.itemVersion(resp.Header), nil
}

// PutFile calls the WOPI PutFile operation on src. An empty lock omits the
// X-WOPI-Lock header; the host then accepts the call only when it holds no
// lock and the stored file is still empty.
func (c *Client) PutFile(ctx context.Context, src, token, lock string, body []byte) (string, error) {
	contents, err := contentsURL(src)
	if err != nil {
		return "", err
	}
	reqURL, err := withAccessToken(contents, token)
	if err != nil {
		return "", err
	}
	header := http.Header{}
	header.Set(headerOverride, "PUT")
	header.Set(headerContentType, "application/octet-stream")
	if lock != "" {
		header.Set(headerLock, lock)
	}

	resp, err := c.send(ctx, OpPutFile, http.MethodPost, reqURL, token, header, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", c.mapErr(OpPutFile, resp, respBody)
	}
	return c.itemVersion(resp.Header), nil
}

// Lock acquires lock on src, or refreshes it when the host already holds
// the same value.
func (c *Client) Lock(ctx context.Context, src, token, lock string) error {
	header := http.Header{}
	header.Set(headerLock, lock)
	return c.lockOp(ctx, OpLock, "LOCK", src, token, header)
}

// GetLock returns the lock value the host currently holds for src. It
// returns the empty string when the file carries no lock.
func (c *Client) GetLock(ctx context.Context, src, token string) (string, error) {
	reqURL, err := withAccessToken(src, token)
	if err != nil {
		return "", err
	}
	header := http.Header{}
	header.Set(headerOverride, "GET_LOCK")

	resp, err := c.send(ctx, OpGetLock, http.MethodPost, reqURL, token, header, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", c.mapErr(OpGetLock, resp, body)
	}
	return resp.Header.Get(headerLock), nil
}

// RefreshLock extends the TTL of the lock the host holds for src. The
// host must already hold exactly this lock value; an expired lock needs
// Lock, not RefreshLock.
func (c *Client) RefreshLock(ctx context.Context, src, token, lock string) error {
	header := http.Header{}
	header.Set(headerLock, lock)
	return c.lockOp(ctx, OpRefreshLock, "REFRESH_LOCK", src, token, header)
}

// Unlock releases lock on src.
func (c *Client) Unlock(ctx context.Context, src, token, lock string) error {
	header := http.Header{}
	header.Set(headerLock, lock)
	return c.lockOp(ctx, OpUnlock, "UNLOCK", src, token, header)
}

// UnlockAndRelock releases oldLock and acquires newLock on src in one
// call.
func (c *Client) UnlockAndRelock(ctx context.Context, src, token, newLock, oldLock string) error {
	header := http.Header{}
	header.Set(headerLock, newLock)
	header.Set(headerOldLock, oldLock)
	return c.lockOp(ctx, OpUnlockAndRelock, "LOCK", src, token, header)
}

func (c *Client) lockOp(ctx context.Context, op, override, src, token string, header http.Header) error {
	reqURL, err := withAccessToken(src, token)
	if err != nil {
		return err
	}
	header.Set(headerOverride, override)

	resp, err := c.send(ctx, op, http.MethodPost, reqURL, token, header, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return c.mapErr(op, resp, body)
	}
	return nil
}

func (c *Client) send(ctx context.Context, op, method, reqURL, token string, header http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("wopiclient: %s: build request: %w", op, err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if c.signer != nil {
		proof, proofOld, timestamp := c.signer.Sign(token, reqURL)
		req.Header.Set(headerProof, proof)
		if proofOld != "" {
			req.Header.Set(headerProofOld, proofOld)
		}
		req.Header.Set(headerTimestamp, timestamp)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wopiclient: %s: %w", op, redactedError{err: err, token: token})
	}
	return resp, nil
}

// redactedError formats like the error it wraps, but with every
// occurrence of the access token replaced. net/http wraps a transport
// failure around the full request URL (a *url.Error), which carries the
// access_token query value; without this, a logged transport error would
// leak it.
type redactedError struct {
	err   error
	token string
}

func (e redactedError) Error() string {
	if e.token == "" {
		return e.err.Error()
	}
	return strings.ReplaceAll(e.err.Error(), e.token, "REDACTED")
}

func (e redactedError) Unwrap() error { return e.err }

// mapErr turns a non-2xx response into an error, preferring the host
// profile's typed mapping and falling back to a generic HTTPError.
func (c *Client) mapErr(op string, resp *http.Response, body []byte) error {
	if c.profile != nil {
		if err := c.profile.MapError(op, resp.StatusCode, resp.Header.Get(headerLock)); err != nil {
			return err
		}
	}
	return HTTPError{Op: op, Status: resp.StatusCode, Body: string(body)}
}

func (c *Client) itemVersion(header http.Header) string {
	if c.profile != nil {
		return c.profile.ItemVersion(header)
	}
	return header.Get("X-WOPI-ItemVersion")
}

// withAccessToken adds the access token as a URL query parameter. A WOPI
// host's proof signature covers the full request URL, so the token must
// travel in the URL rather than only in an Authorization header.
func withAccessToken(rawURL, token string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("wopiclient: parse WOPISrc: %w", err)
	}
	q := u.Query()
	q.Set("access_token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// contentsURL appends /contents to src's path via url.Parse, not string
// concatenation, so a WOPISrc that already carries a query string
// (Drive includes one on occasion) survives instead of landing inside
// the appended literal.
func contentsURL(src string) (string, error) {
	u, err := url.Parse(src)
	if err != nil {
		return "", fmt.Errorf("wopiclient: parse WOPISrc: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/contents"
	return u.String(), nil
}
