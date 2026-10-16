package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/content/local"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/docker/distribution"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type (
	// LocalBlobStore writes and stats blobs in an app store, providing the subset of
	// a registry blob store needed to build an app manifest without a registry.
	LocalBlobStore struct {
		cs content.Store
	}
)

func NewLocalBlobStore(storeRoot string) (*LocalBlobStore, error) {
	cs, err := local.NewStore(storeRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to open app store %s: %w", storeRoot, err)
	}
	return &LocalBlobStore{cs: cs}, nil
}

func (s *LocalBlobStore) Stat(ctx context.Context, dgst digest.Digest) (distribution.Descriptor, error) {
	info, err := s.cs.Info(ctx, dgst)
	if errdefs.IsNotFound(err) {
		return distribution.Descriptor{}, distribution.ErrBlobUnknown
	}
	if err != nil {
		return distribution.Descriptor{}, fmt.Errorf("failed to stat blob %s: %w", dgst, err)
	}
	return distribution.Descriptor{Digest: info.Digest, Size: info.Size}, nil
}

func (s *LocalBlobStore) Put(ctx context.Context, mediaType string, p []byte) (distribution.Descriptor, error) {
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(p), Size: int64(len(p))}
	if err := content.WriteBlob(ctx, s.cs, desc.Digest.String(), bytes.NewReader(p), desc); err != nil {
		return distribution.Descriptor{}, fmt.Errorf("failed to write blob %s: %w", desc.Digest, err)
	}
	return distribution.Descriptor{MediaType: mediaType, Digest: desc.Digest, Size: desc.Size}, nil
}

// IngestOCILayout copies every blob reachable from the single image in an OCI layout directory
// into the store and returns the descriptor of that image's root (an index or a manifest).
// Each blob's digest is verified while it is copied.
func (s *LocalBlobStore) IngestOCILayout(ctx context.Context, layoutDir string) (*ocispec.Descriptor, error) {
	src, err := local.NewStore(layoutDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open OCI layout %s: %w", layoutDir, err)
	}
	root, err := readOCILayoutRoot(layoutDir)
	if err != nil {
		return nil, err
	}
	copyHandler := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		ra, err := src.ReaderAt(ctx, desc)
		if err != nil {
			return nil, fmt.Errorf("failed to read blob %s from OCI layout: %w", desc.Digest, err)
		}
		defer ra.Close()
		if err := content.WriteBlob(ctx, s.cs, desc.Digest.String(), content.NewReader(ra), desc); err != nil {
			return nil, fmt.Errorf("failed to ingest blob %s: %w", desc.Digest, err)
		}
		return nil, nil
	})
	if err := images.Walk(ctx, images.Handlers(copyHandler, images.ChildrenHandler(src)), *root); err != nil {
		return nil, err
	}
	return root, nil
}

func readOCILayoutRoot(layoutDir string) (*ocispec.Descriptor, error) {
	b, err := os.ReadFile(filepath.Join(layoutDir, ocispec.ImageIndexFile))
	if err != nil {
		return nil, fmt.Errorf("failed to read OCI layout index: %w", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(b, &index); err != nil {
		return nil, fmt.Errorf("failed to parse OCI layout index: %w", err)
	}
	if len(index.Manifests) != 1 {
		return nil, fmt.Errorf("expected exactly one image in OCI layout %s, found %d", layoutDir, len(index.Manifests))
	}
	root := index.Manifests[0]
	if !images.IsIndexType(root.MediaType) && !images.IsManifestType(root.MediaType) {
		return nil, errors.New("unsupported OCI layout root media type: " + root.MediaType)
	}
	// the annotations, like the image creation time, describe the layout entry, not the image
	root.Annotations = nil
	return &root, nil
}
