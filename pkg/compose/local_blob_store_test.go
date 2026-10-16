package compose

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/distribution"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func writeLayoutBlob(t *testing.T, layoutDir string, mediaType string, b []byte) ocispec.Descriptor {
	t.Helper()
	d := digest.FromBytes(b)
	blobsDir := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobsDir, d.Encoded()), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(b))}
}

func writeJSONLayoutBlob(t *testing.T, layoutDir string, mediaType string, v interface{}) ocispec.Descriptor {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return writeLayoutBlob(t, layoutDir, mediaType, b)
}

// newTestOCILayout creates an OCI layout holding a two platform image index and returns
// the index descriptor and the digests of all blobs it refers to, including the index.
func newTestOCILayout(t *testing.T, layoutDir string) (ocispec.Descriptor, []digest.Digest) {
	t.Helper()
	var all []digest.Digest
	var manifests []ocispec.Descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		layer := writeLayoutBlob(t, layoutDir, ocispec.MediaTypeImageLayerGzip, []byte("layer-"+arch))
		config := writeJSONLayoutBlob(t, layoutDir, ocispec.MediaTypeImageConfig,
			ocispec.Image{Platform: ocispec.Platform{OS: "linux", Architecture: arch}})
		m := writeJSONLayoutBlob(t, layoutDir, ocispec.MediaTypeImageManifest, ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
			Config: config, Layers: []ocispec.Descriptor{layer},
		})
		m.Platform = &ocispec.Platform{OS: "linux", Architecture: arch}
		manifests = append(manifests, m)
		all = append(all, layer.Digest, config.Digest, m.Digest)
	}
	index := writeJSONLayoutBlob(t, layoutDir, ocispec.MediaTypeImageIndex, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: manifests,
	})
	all = append(all, index.Digest)
	entry := index
	entry.Annotations = map[string]string{ocispec.AnnotationRefName: "latest"}
	writeIndexFile(t, layoutDir, []ocispec.Descriptor{entry})
	return index, all
}

func writeIndexFile(t *testing.T, layoutDir string, manifests []ocispec.Descriptor) {
	t.Helper()
	b, err := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: manifests})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, ocispec.ImageIndexFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLocalBlobStorePutAndStat(t *testing.T) {
	ctx := context.Background()
	s, err := NewLocalBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, digest.FromString("missing")); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Fatalf("expected ErrBlobUnknown, got: %v", err)
	}
	desc, err := s.Put(ctx, "application/json", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	// putting the same content again must succeed
	if _, err := s.Put(ctx, "application/json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	stat, err := s.Stat(ctx, desc.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size != 2 || stat.Digest != digest.FromString("{}") {
		t.Errorf("unexpected blob stat: %+v", stat)
	}
}

func TestLocalBlobStoreIngestOCILayout(t *testing.T) {
	ctx := context.Background()
	layoutDir := t.TempDir()
	index, blobs := newTestOCILayout(t, layoutDir)
	storeRoot := t.TempDir()
	s, err := NewLocalBlobStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.IngestOCILayout(ctx, layoutDir)
	if err != nil {
		t.Fatal(err)
	}
	if root.Digest != index.Digest || root.MediaType != ocispec.MediaTypeImageIndex || root.Annotations != nil {
		t.Errorf("unexpected root descriptor: %+v", root)
	}
	for _, d := range blobs {
		if _, err := os.Stat(filepath.Join(GetBlobsRootFor(storeRoot), d.Encoded())); err != nil {
			t.Errorf("blob %s is not in the store: %s", d, err)
		}
	}
}

func TestLocalBlobStoreIngestOCILayoutRejectsCorruptBlob(t *testing.T) {
	layoutDir := t.TempDir()
	_, blobs := newTestOCILayout(t, layoutDir)
	// corrupt the amd64 layer
	if err := os.WriteFile(filepath.Join(layoutDir, "blobs", "sha256", blobs[0].Encoded()), []byte("layer-xxxxx"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewLocalBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestOCILayout(context.Background(), layoutDir); err == nil {
		t.Fatal("expected ingesting a corrupt blob to fail")
	}
}

func TestLocalBlobStoreIngestOCILayoutRequiresSingleImage(t *testing.T) {
	layoutDir := t.TempDir()
	index, _ := newTestOCILayout(t, layoutDir)
	writeIndexFile(t, layoutDir, []ocispec.Descriptor{index, index})
	s, err := NewLocalBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestOCILayout(context.Background(), layoutDir); err == nil {
		t.Fatal("expected a layout with two images to be rejected")
	}
}
