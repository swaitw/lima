package downloader

import (
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	"gotest.tools/v3/assert"
)

// TODO: create a localhost HTTP server to serve the test contents without Internet
const (
	dummyRemoteFileURL    = "https://raw.githubusercontent.com/lima-vm/lima/7459f4587987ed014c372f17b82de1817feffa2e/README.md"
	dummyRemoteFileDigest = "sha256:58d2de96f9d91f0acd93cb1e28bf7c42fc86079037768d6aa63b4e7e7b3c9be0"
)

func TestDownloadRemote(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	t.Run("without cache", func(t *testing.T) {
		t.Run("without digest", func(t *testing.T) {
			localPath := filepath.Join(t.TempDir(), t.Name())
			r, err := Download(localPath, dummyRemoteFileURL)
			assert.NilError(t, err)
			assert.Equal(t, StatusDownloaded, r.Status)

			// download again, make sure StatusSkippedIsReturned
			r, err = Download(localPath, dummyRemoteFileURL)
			assert.NilError(t, err)
			assert.Equal(t, StatusSkipped, r.Status)
		})
		t.Run("with digest", func(t *testing.T) {
			wrongDigest := digest.Digest("sha256:8313944efb4f38570c689813f288058b674ea6c487017a5a4738dc674b65f9d9")
			localPath := filepath.Join(t.TempDir(), t.Name())
			_, err := Download(localPath, dummyRemoteFileURL, WithExpectedDigest(wrongDigest))
			assert.ErrorContains(t, err, "expected digest")

			r, err := Download(localPath, dummyRemoteFileURL, WithExpectedDigest(dummyRemoteFileDigest))
			assert.NilError(t, err)
			assert.Equal(t, StatusDownloaded, r.Status)

			r, err = Download(localPath, dummyRemoteFileURL, WithExpectedDigest(dummyRemoteFileDigest))
			assert.NilError(t, err)
			assert.Equal(t, StatusSkipped, r.Status)
		})
	})
	t.Run("with cache", func(t *testing.T) {
		cacheDir := filepath.Join(t.TempDir(), "cache")
		localPath := filepath.Join(t.TempDir(), t.Name())
		r, err := Download(localPath, dummyRemoteFileURL, WithExpectedDigest(dummyRemoteFileDigest), WithCacheDir(cacheDir))
		assert.NilError(t, err)
		assert.Equal(t, StatusDownloaded, r.Status)

		r, err = Download(localPath, dummyRemoteFileURL, WithExpectedDigest(dummyRemoteFileDigest), WithCacheDir(cacheDir))
		assert.NilError(t, err)
		assert.Equal(t, StatusSkipped, r.Status)

		localPath2 := localPath + "-2"
		r, err = Download(localPath2, dummyRemoteFileURL, WithExpectedDigest(dummyRemoteFileDigest), WithCacheDir(cacheDir))
		assert.NilError(t, err)
		assert.Equal(t, StatusUsedCache, r.Status)
	})
	t.Run("caching-only mode", func(t *testing.T) {
		_, err := Download("", dummyRemoteFileURL, WithExpectedDigest(dummyRemoteFileDigest))
		assert.ErrorContains(t, err, "cache directory to be specified")

		cacheDir := filepath.Join(t.TempDir(), "cache")
		r, err := Download("", dummyRemoteFileURL, WithExpectedDigest(dummyRemoteFileDigest), WithCacheDir(cacheDir))
		assert.NilError(t, err)
		assert.Equal(t, StatusDownloaded, r.Status)

		r, err = Download("", dummyRemoteFileURL, WithExpectedDigest(dummyRemoteFileDigest), WithCacheDir(cacheDir))
		assert.NilError(t, err)
		assert.Equal(t, StatusUsedCache, r.Status)

		localPath := filepath.Join(t.TempDir(), t.Name())
		r, err = Download(localPath, dummyRemoteFileURL, WithExpectedDigest(dummyRemoteFileDigest), WithCacheDir(cacheDir))
		assert.NilError(t, err)
		assert.Equal(t, StatusUsedCache, r.Status)
	})
}
