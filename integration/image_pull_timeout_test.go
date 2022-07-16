/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	criconfig "github.com/containerd/containerd/pkg/cri/config"
	criserver "github.com/containerd/containerd/pkg/cri/server"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var (
	defaultImagePullProgressTimeout = 5 * time.Second
	pullProgressTestImageName       = "ghcr.io/containerd/registry:2.7"
)

func TestCRIImagePullTimeout(t *testing.T) {
	t.Parallel()

	// TODO(fuweid): Test it in Windows.
	if runtime.GOOS != "linux" {
		t.Skip()
	}

	t.Run("HoldingContentOpenWriter", testCRIImagePullTimeoutByHoldingContentOpenWriter)
	t.Run("NoDataTransferred", testCRIImagePullTimeoutByNoDataTransferred)
}

// testCRIImagePullTimeoutByHoldingContentOpenWriter tests that
//
//  It should not cancel if there is no active http requests.
//
// When there are several pulling requests for the same blob content, there
// will only one active http request. It is singleflight. For the waiting pulling
// request, we should not cancel.
func testCRIImagePullTimeoutByHoldingContentOpenWriter(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	cli := buildLocalContainerdClient(t, tmpDir)

	criService, err := initLocalCRIPlugin(cli, tmpDir, criconfig.Registry{})
	assert.NoError(t, err)

	ctx := namespaces.WithNamespace(context.Background(), k8sNamespace)
	contentStore := cli.ContentStore()

	// imageIndexJSON is the manifest of ghcr.io/containerd/registry:2.7.
	var imageIndexJSON = `
{
  "manifests": [
    {
      "digest": "sha256:b0b8dd398630cbb819d9a9c2fbd50561370856874b5d5d935be2e0af07c0ff4c",
      "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
      "platform": {
        "architecture": "amd64",
        "os": "linux"
      },
      "size": 1363
    },
    {
      "digest": "sha256:6de6b4d5063876c92220d0438ae6068c778d9a2d3845b3d5c57a04a307998df6",
      "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
      "platform": {
        "architecture": "arm",
        "os": "linux",
        "variant": "v6"
      },
      "size": 1363
    },
    {
      "digest": "sha256:c11a277a91045f91866550314a988f937366bc2743859aa0f6ec8ef57b0458ce",
      "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
      "platform": {
        "architecture": "arm64",
        "os": "linux",
        "variant": "v8"
      },
      "size": 1363
    }
  ],
  "mediaType": "application/vnd.docker.distribution.manifest.list.v2+json",
  "schemaVersion": 2
}`
	var index ocispec.Index
	assert.NoError(t, json.Unmarshal([]byte(imageIndexJSON), &index))

	var manifestWriters = []io.Closer{}

	cleanupWriters := func() {
		for _, closer := range manifestWriters {
			closer.Close()
		}
		manifestWriters = manifestWriters[:0]
	}
	defer cleanupWriters()

	// hold the writer by the desc
	for _, desc := range index.Manifests {
		writer, err := content.OpenWriter(ctx, contentStore,
			content.WithDescriptor(desc),
			content.WithRef(fmt.Sprintf("manifest-%v", desc.Digest)),
		)
		assert.NoError(t, err, "failed to locked manifest")

		t.Logf("locked the manifest %+v", desc)
		manifestWriters = append(manifestWriters, writer)
	}

	errCh := make(chan error)
	go func() {
		defer close(errCh)

		_, err := criService.PullImage(ctx, &runtimeapi.PullImageRequest{
			Image: &runtimeapi.ImageSpec{
				Image: pullProgressTestImageName,
			},
		})
		errCh <- err
	}()

	select {
	case <-time.After(defaultImagePullProgressTimeout * 5):
		// release the lock
		cleanupWriters()
	case err := <-errCh:
		t.Fatalf("PullImage should not return because the manifest has been locked, but got error=%v", err)
	}
	assert.NoError(t, <-errCh)
}

// testCRIImagePullTimeoutByNoDataTransferred tests that
//
//   It should fail because there is no data transferred in open http request.
//
// The case uses the local mirror registry to forward request with circuit
// breaker. If the local registry has transferred a certain amount of data in
// connection, it will enable circuit breaker and sleep for a while. For the
// CRI plugin, it will see there is no data transported. And then cancel the
// pulling request when timeout.
//
// This case uses ghcr.io/containerd/registry:2.7 which has one layer > 3MB.
// The circuit breaker will enable after transferred 3MB in one connection.
func testCRIImagePullTimeoutByNoDataTransferred(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	cli := buildLocalContainerdClient(t, tmpDir)

	mirrorSrv := newMirrorRegistryServer(mirrorRegistryServerConfig{
		limitedBytesPerConn: 1024 * 1024 * 3, // 3MB
		retryAfter:          100 * time.Second,
		targetURL: &url.URL{
			Scheme: "https",
			Host:   "ghcr.io",
		},
	})

	ts := setupLocalMirrorRegistry(mirrorSrv)
	defer ts.Close()

	mirrorURL, err := url.Parse(ts.URL)
	assert.NoError(t, err)

	var hostTomlContent = fmt.Sprintf(`
[host."%s"]
  capabilities = ["pull", "resolve", "push"]
  skip_verify = true
`, mirrorURL.String())

	hostCfgDir := filepath.Join(tmpDir, "registrycfg", mirrorURL.Host)
	assert.NoError(t, os.MkdirAll(hostCfgDir, 0600))

	err = os.WriteFile(filepath.Join(hostCfgDir, "hosts.toml"), []byte(hostTomlContent), 0600)
	assert.NoError(t, err)

	ctx := namespaces.WithNamespace(context.Background(), k8sNamespace)
	for idx, registryCfg := range []criconfig.Registry{
		{
			ConfigPath: filepath.Dir(hostCfgDir),
		},
		// TODO(fuweid):
		//
		// Both Mirrors and Configs are deprecated in the future. And
		// this registryCfg should also be removed at that time.
		{
			Mirrors: map[string]criconfig.Mirror{
				mirrorURL.Host: {
					Endpoints: []string{mirrorURL.String()},
				},
			},
			Configs: map[string]criconfig.RegistryConfig{
				mirrorURL.Host: {
					TLS: &criconfig.TLSConfig{
						InsecureSkipVerify: true,
					},
				},
			},
		},
	} {
		criService, err := initLocalCRIPlugin(cli, tmpDir, registryCfg)
		assert.NoError(t, err)

		dctx, _, err := cli.WithLease(ctx)
		assert.NoError(t, err)

		_, err = criService.PullImage(dctx, &runtimeapi.PullImageRequest{
			Image: &runtimeapi.ImageSpec{
				Image: fmt.Sprintf("%s/%s", mirrorURL.Host, "containerd/registry:2.7"),
			},
		})

		assert.Equal(t, errors.Unwrap(err), context.Canceled, "[%v] expected canceled error, but got (%v)", idx, err)
		assert.Equal(t, mirrorSrv.limiter.clearHitCircuitBreaker(), true, "[%v] expected to hit circuit breaker", idx)

		// cleanup the temp data by sync delete
		lid, ok := leases.FromContext(dctx)
		assert.Equal(t, ok, true)
		err = cli.LeasesService().Delete(ctx, leases.Lease{ID: lid}, leases.SynchronousDelete)
		assert.NoError(t, err)
	}
}

func setupLocalMirrorRegistry(srv *mirrorRegistryServer) *httptest.Server {
	return httptest.NewServer(srv)
}

func newMirrorRegistryServer(cfg mirrorRegistryServerConfig) *mirrorRegistryServer {
	return &mirrorRegistryServer{
		client:    http.DefaultClient,
		limiter:   newIOCopyLimiter(cfg.limitedBytesPerConn, cfg.retryAfter),
		targetURL: cfg.targetURL,
	}
}

type mirrorRegistryServerConfig struct {
	limitedBytesPerConn int
	retryAfter          time.Duration
	targetURL           *url.URL
}

type mirrorRegistryServer struct {
	client    *http.Client
	limiter   *ioCopyLimiter
	targetURL *url.URL
}

func (srv *mirrorRegistryServer) ServeHTTP(respW http.ResponseWriter, req *http.Request) {
	originalURL := &url.URL{
		Scheme: "http",
		Host:   req.Host,
	}

	req.URL.Host = srv.targetURL.Host
	req.URL.Scheme = srv.targetURL.Scheme
	req.Host = srv.targetURL.Host

	req.RequestURI = ""
	fresp, err := srv.client.Do(req)
	if err != nil {
		http.Error(respW, fmt.Sprintf("failed to mirror request: %v", err), http.StatusBadGateway)
		return
	}
	defer fresp.Body.Close()

	// copy header and modified that authentication value
	authKey := http.CanonicalHeaderKey("WWW-Authenticate")
	for key, vals := range fresp.Header {
		replace := (key == authKey)

		for _, val := range vals {
			if replace {
				val = strings.Replace(val, srv.targetURL.String(), originalURL.String(), -1)
				val = strings.Replace(val, srv.targetURL.Host, originalURL.Host, -1)
			}
			respW.Header().Add(key, val)
		}
	}

	respW.WriteHeader(fresp.StatusCode)
	if err := srv.limiter.limitedCopy(req.Context(), respW, fresp.Body); err != nil {
		log.G(req.Context()).Errorf("failed to forward response: %v", err)
	}
}

var (
	defaultBufSize = 1024 * 4

	bufPool = sync.Pool{
		New: func() interface{} {
			buffer := make([]byte, defaultBufSize)
			return &buffer
		},
	}
)

func newIOCopyLimiter(limitedBytesPerConn int, retryAfter time.Duration) *ioCopyLimiter {
	return &ioCopyLimiter{
		limitedBytes: limitedBytesPerConn,
		retryAfter:   retryAfter,
	}
}

// ioCopyLimiter will postpone the data transfer after limitedBytes has been
// transferred, like circuit breaker.
type ioCopyLimiter struct {
	limitedBytes      int
	retryAfter        time.Duration
	hitCircuitBreaker bool
}

func (l *ioCopyLimiter) clearHitCircuitBreaker() bool {
	last := l.hitCircuitBreaker
	l.hitCircuitBreaker = false
	return last
}

func (l *ioCopyLimiter) limitedCopy(ctx context.Context, dst io.Writer, src io.Reader) error {
	var (
		bufRef  = bufPool.Get().(*[]byte)
		buf     = *bufRef
		timer   = time.NewTimer(0)
		written int64
	)

	defer bufPool.Put(bufRef)

	stopTimer := func(t *time.Timer, needRecv bool) {
		if !t.Stop() && needRecv {
			<-t.C
		}
	}

	waitForRetry := func(t *time.Timer, delay time.Duration) error {
		needRecv := true

		t.Reset(delay)
		select {
		case <-t.C:
			needRecv = false
		case <-ctx.Done():
			return ctx.Err()
		}
		stopTimer(t, needRecv)
		return nil
	}

	stopTimer(timer, true)
	defer timer.Stop()
	for {
		if written > int64(l.limitedBytes) {
			l.hitCircuitBreaker = true

			log.G(ctx).Warnf("after %v bytes transferred, enable breaker and retransfer after %v", written, l.retryAfter)
			if wer := waitForRetry(timer, l.retryAfter); wer != nil {
				return wer
			}

			written = 0
			l.hitCircuitBreaker = false
		}

		nr, er := io.ReadAtLeast(src, buf, len(buf))
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				return ew
			}
			if nr != nw {
				return io.ErrShortWrite
			}
		}
		if er != nil {
			if er != io.EOF && er != io.ErrUnexpectedEOF {
				return er
			}
			break
		}
	}
	return nil
}

// initLocalCRIPlugin uses containerd.Client to init CRI plugin.
//
// NOTE: We don't need to start the CRI plugin here because we just need the
// ImageService API.
func initLocalCRIPlugin(client *containerd.Client, tmpDir string, registryCfg criconfig.Registry) (criserver.CRIService, error) {
	containerdRootDir := filepath.Join(tmpDir, "root")
	criWorkDir := filepath.Join(tmpDir, "cri-plugin")

	cfg := criconfig.Config{
		PluginConfig: criconfig.PluginConfig{
			ContainerdConfig: criconfig.ContainerdConfig{
				Snapshotter: containerd.DefaultSnapshotter,
			},
			Registry:                 registryCfg,
			ImagePullProgressTimeout: defaultImagePullProgressTimeout.String(),
		},
		ContainerdRootDir: containerdRootDir,
		RootDir:           filepath.Join(criWorkDir, "root"),
		StateDir:          filepath.Join(criWorkDir, "state"),
	}
	return criserver.NewCRIService(cfg, client)
}
