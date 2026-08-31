// Package driveintegration_test runs internal/wopiclient against the real
// dockerized Drive from e2e/compose.yaml. It needs the stack up (`make
// e2e-up`) and Node on PATH.
//
// It seeds its own items by running e2e/scripts/seed.mjs through os/exec,
// once per item it needs, rather than reading a JSON fixture path from an
// env var: a subprocess call keeps every subtest's fixture fresh and
// independent, and needs no extra Makefile plumbing to produce the file
// beforehand.
//
//go:build driveintegration

package driveintegration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/config"
	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/proof"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

const (
	requestTimeout = 30 * time.Second
	rsaKeyBits     = 2048
)

// seedResult is e2e/scripts/seed.mjs's JSON stdout.
type seedResult struct {
	ItemID         string `json:"itemId"`
	LaunchURL      string `json:"launchUrl"`
	AccessToken    string `json:"accessToken"`
	AccessTokenTTL int64  `json:"accessTokenTtl"`
	Filename       string `json:"filename"`
	SceneBase64    string `json:"sceneBase64"`
}

// scene decodes the exact bytes seed.mjs uploaded, so a test never has to
// guess the script's scene content.
func (r seedResult) scene(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(r.SceneBase64)
	if err != nil {
		t.Fatalf("decode sceneBase64: %v", err)
	}
	return data
}

// src returns the WOPISrc Drive computed for the item: the CheckFileInfo
// URL a WOPI client calls. Drive publishes it as a query parameter on
// the launch URL (wopi/utils/__init__.py compute_wopi_launch_url).
func (r seedResult) src(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(r.LaunchURL)
	if err != nil {
		t.Fatalf("parse launchUrl %q: %v", r.LaunchURL, err)
	}
	src := u.Query().Get("WOPISrc")
	if src == "" {
		t.Fatalf("launchUrl %q carries no WOPISrc", r.LaunchURL)
	}
	return src
}

// seed runs e2e/scripts/seed.mjs and decodes its JSON stdout. args are
// extra CLI flags, e.g. "--empty" or "--filename".
func seed(t *testing.T, args ...string) seedResult {
	t.Helper()

	scriptPath := filepath.Join(repoRoot(t), "e2e", "scripts", "seed.mjs")
	cmd := exec.Command("node", append([]string{scriptPath}, args...)...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("seed.mjs %v: %v\n%s", args, err, stderr.String())
	}
	t.Logf("seed.mjs %v:\n%s", args, stderr.String())

	var result seedResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("seed.mjs %v: decode stdout: %v\nstdout:\n%s", args, err, stdout.String())
	}
	return result
}

// repoRoot resolves the repository root from this source file's own path,
// so seed() finds seed.mjs whether `go test` or a directly executed
// compiled test binary (see e2e/README.md's integration test section) set
// the working directory to something else.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("driveintegration: cannot resolve this test file's path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// realSigner loads the proof keypair the running excalidraw-wopi binary
// persisted. It reads config the same way cmd/excalidraw-wopi does, so it
// resolves the same EXCALIDRAW_WOPI_PROOF_KEY_PATH (or the same default,
// /var/lib/excalidraw-wopi/proof-key.pem — see e2e/env/excalidraw.env,
// which sets neither that var nor EXCALIDRAW_WOPI_PROOF_KEY_PEM) and reads
// the same file the binary wrote on first start (e2e/scripts/e2e-up.sh
// starts the binary with that env file). Drive rejects a proof signed
// with any other key, since its cached configuration only trusts the key
// our /hosting/discovery published.
func realSigner(t *testing.T) requestSigner {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	keys, err := proof.Load(cfg)
	if err != nil {
		t.Fatalf("load proof key from %s: %v", cfg.ProofKeyPath, err)
	}
	return requestSigner{keys: keys}
}

// strangerSigner returns a signer built from a freshly generated keypair
// that Drive never saw, for the proof-rejection negative test.
func strangerSigner(t *testing.T) requestSigner {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		t.Fatalf("generate stranger keypair: %v", err)
	}
	return requestSigner{keys: &proof.KeySet{Current: key, Old: key}}
}

// requestSigner adapts proof.KeySet (which also takes a timestamp) to
// wopiclient.RequestSigner.
type requestSigner struct{ keys *proof.KeySet }

func (s requestSigner) Sign(accessToken, reqURL string) (string, string, string) {
	return s.keys.SignRequest(accessToken, reqURL, time.Now())
}

func newClient(signer wopiclient.RequestSigner) *wopiclient.Client {
	httpClient := &http.Client{Timeout: requestTimeout}
	return wopiclient.New(httpClient, signer, hostadapter.NewDrive())
}

func TestDriveIntegration(t *testing.T) {
	signer := realSigner(t)
	client := newClient(signer)
	ctx := context.Background()

	main := seed(t, "--filename", "board-main.excalidraw")
	empty := seed(t, "--empty", "--filename", "board-empty.excalidraw")
	lockItem := seed(t, "--filename", "board-lock.excalidraw")

	t.Run("CheckFileInfo", func(t *testing.T) {
		info, err := client.CheckFileInfo(ctx, main.src(t), main.AccessToken)
		if err != nil {
			t.Fatalf("CheckFileInfo: %v", err)
		}
		if info.BaseFileName != main.Filename {
			t.Errorf("BaseFileName = %q, want %q", info.BaseFileName, main.Filename)
		}
		if !info.UserCanWrite {
			t.Error("UserCanWrite = false, want true")
		}
		if info.Version == "" {
			t.Error("Version is empty, want a version marker")
		}
		if want := int64(len(main.scene(t))); info.Size != want {
			t.Errorf("Size = %d, want %d", info.Size, want)
		}
	})

	t.Run("GetFile", func(t *testing.T) {
		info, err := client.CheckFileInfo(ctx, main.src(t), main.AccessToken)
		if err != nil {
			t.Fatalf("CheckFileInfo: %v", err)
		}

		body, version, err := client.GetFile(ctx, main.src(t), main.AccessToken)
		if err != nil {
			t.Fatalf("GetFile: %v", err)
		}
		defer body.Close()

		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("read GetFile body: %v", err)
		}
		if !bytes.Equal(data, main.scene(t)) {
			t.Errorf("GetFile body = %q, want %q", data, main.scene(t))
		}
		if version != info.Version {
			t.Errorf("GetFile X-WOPI-ItemVersion = %q, want CheckFileInfo Version %q", version, info.Version)
		}
	})

	// An unlocked PutFile succeeds only while the stored file is still 0
	// bytes. empty and lockItem cover both sides of that rule; lockItem
	// then carries on, still unlocked, into LockLifecycle.
	t.Run("EmptyFileRule", func(t *testing.T) {
		t.Run("UnlockedPutFileOnEmptyFileSucceeds", func(t *testing.T) {
			version, err := client.PutFile(ctx, empty.src(t), empty.AccessToken, "", []byte("first save"))
			if err != nil {
				t.Fatalf("PutFile: %v", err)
			}
			if version == "" {
				t.Error("PutFile returned an empty X-WOPI-ItemVersion")
			}
		})

		t.Run("UnlockedPutFileOnNonEmptyFileConflicts", func(t *testing.T) {
			_, err := client.PutFile(ctx, lockItem.src(t), lockItem.AccessToken, "", []byte("should not land"))
			var conflict wopiclient.ErrLockConflict
			if !errors.As(err, &conflict) {
				t.Fatalf("PutFile error = %v, want ErrLockConflict", err)
			}
			if conflict.CurrentLock != "" {
				t.Errorf("CurrentLock = %q, want empty", conflict.CurrentLock)
			}
		})
	})

	t.Run("LockLifecycle", func(t *testing.T) {
		src := lockItem.src(t)
		token := lockItem.AccessToken

		baseline, err := client.CheckFileInfo(ctx, src, token)
		if err != nil {
			t.Fatalf("CheckFileInfo baseline: %v", err)
		}

		if err := client.Lock(ctx, src, token, "L1"); err != nil {
			t.Fatalf("Lock(L1): %v", err)
		}

		newVersion, err := client.PutFile(ctx, src, token, "L1", []byte(`{"elements":["updated"]}`))
		if err != nil {
			t.Fatalf("PutFile(L1): %v", err)
		}
		if newVersion == baseline.Version {
			t.Errorf("Version did not move: still %q after a locked PutFile", newVersion)
		}

		_, err = client.PutFile(ctx, src, token, "WRONG", []byte("should not land"))
		var conflict wopiclient.ErrLockConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("PutFile(WRONG) error = %v, want ErrLockConflict", err)
		}
		if conflict.CurrentLock != "L1" {
			t.Errorf("CurrentLock = %q, want L1", conflict.CurrentLock)
		}

		if err := client.Lock(ctx, src, token, "L1"); err != nil {
			t.Fatalf("Lock(L1) refresh: %v", err)
		}

		lock, err := client.GetLock(ctx, src, token)
		if err != nil {
			t.Fatalf("GetLock: %v", err)
		}
		if lock != "L1" {
			t.Errorf("GetLock = %q, want L1", lock)
		}

		if err := client.UnlockAndRelock(ctx, src, token, "L2", "L1"); err != nil {
			t.Fatalf("UnlockAndRelock(L2, old L1): %v", err)
		}

		if err := client.Unlock(ctx, src, token, "L2"); err != nil {
			t.Fatalf("Unlock(L2): %v", err)
		}
	})

	t.Run("Auth", func(t *testing.T) {
		_, err := client.CheckFileInfo(ctx, main.src(t), "not-a-real-access-token")
		if !errors.As(err, &wopiclient.ErrTokenRejected{}) {
			t.Fatalf("CheckFileInfo with a garbage token: %v, want ErrTokenRejected", err)
		}
	})

	t.Run("Proof", func(t *testing.T) {
		strangerClient := newClient(strangerSigner(t))
		_, err := strangerClient.CheckFileInfo(ctx, main.src(t), main.AccessToken)
		if !errors.As(err, &wopiclient.ErrProofRejected{}) {
			t.Fatalf("CheckFileInfo signed by a stranger keypair: %v, want ErrProofRejected", err)
		}
	})
}
