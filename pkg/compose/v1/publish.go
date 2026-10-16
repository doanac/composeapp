//go:build publish

package v1

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/docker/distribution"
	"github.com/docker/distribution/manifest/manifestlist"
	"github.com/docker/distribution/manifest/ocischema"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/pkg/archive"
	"github.com/foundriesio/composeapp/pkg/compose"
	"gopkg.in/yaml.v3"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/loader"
	"github.com/compose-spec/compose-go/types"
	"github.com/foundriesio/composeapp/internal"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/opencontainers/go-digest"
)

func loadProj(ctx context.Context, appName string, file string, content []byte) (*types.Project, error) {
	env := make(map[string]string)
	for _, val := range os.Environ() {
		parts := strings.Split(val, "=")
		env[parts[0]] = parts[1]
	}

	var files []types.ConfigFile
	files = append(files, types.ConfigFile{Filename: file, Content: content})
	return loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  ".",
		ConfigFiles: files,
		Environment: env,
	}, func(options *loader.Options) {
		options.SetProjectName(appName, true)
	})
}

// ImageResolver pins the image of a compose service to a digest URI.
type ImageResolver interface {
	ResolveImage(ctx context.Context, svc types.ServiceConfig) (string, error)
}

type registryImageResolver struct {
	regc         internal.RegistryClient
	pinnedImages map[string]digest.Digest
}

// NewRegistryImageResolver returns a resolver that pins service images to the digests
// their tags refer to in a container registry.
func NewRegistryImageResolver(pinnedImages map[string]digest.Digest) ImageResolver {
	return &registryImageResolver{regc: internal.NewRegistryClient(), pinnedImages: pinnedImages}
}

// LoadAndPinApp reads a compose project file and returns its YAML config with every service
// image pinned by the resolver and every service labeled with the hash of its config.
func LoadAndPinApp(ctx context.Context, appName string, file string,
	resolver ImageResolver) (map[string]interface{}, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read compose file: %w", err)
	}
	config, err := loader.ParseYAML(b)
	if err != nil {
		return nil, fmt.Errorf("failed to parse compose file: %w", err)
	}
	proj, err := loadProj(ctx, appName, file, b)
	if err != nil {
		return nil, fmt.Errorf("failed to load compose project: %w", err)
	}
	svcs, ok := config["services"].(map[string]interface{})
	if !ok {
		return nil, errors.New("Unable to find 'services' section of composetypes file")
	}

	fmt.Println("= Pinning service images...")
	if err := pinServiceImages(ctx, svcs, proj, resolver); err != nil {
		return nil, err
	}
	fmt.Println("== Hashing services...")
	if err := pinServiceConfigs(svcs, proj); err != nil {
		return nil, err
	}
	return config, nil
}

func DoPublish(ctx context.Context, appName string, file, target, digestFile string, dryRun bool, archList []string,
	pinnedImages map[string]digest.Digest, layersMetaFile string, createAppLayersManifest bool) error {
	config, err := LoadAndPinApp(ctx, appName, file, NewRegistryImageResolver(pinnedImages))
	if err != nil {
		return err
	}
	svcs := config["services"].(map[string]interface{})

	fmt.Println("= Getting app layers metadata...")
	appLayers, err := compose.GetLayers(ctx, svcs, archList)
	if err != nil {
		return err
	}

	if len(appLayers) == 0 {
		return fmt.Errorf("none of the factory architectures %q are supported by App images", archList)
	}

	var layerManifests []distribution.Descriptor
	if createAppLayersManifest {
		fmt.Println("= Posting app layers manifests...")
		layerManifests, err = compose.PostAppLayersManifests(ctx, target, appLayers, dryRun)
		if err != nil {
			return err
		}
	}

	var appLayersMetaBytes []byte
	if len(layersMetaFile) > 0 {
		fmt.Println("= Getting app layers metadata...")
		appLayersMetaBytes, err = compose.GetAppLayersMeta(layersMetaFile, appLayers)
		if err != nil {
			return fmt.Errorf("= Failed to get app layers metadata: %s\n", err.Error())
		}
	}

	fmt.Println("= Publishing app...")
	dgst, err := createAndPublishApp(ctx, config, target, dryRun, layerManifests, appLayersMetaBytes)
	if err != nil {
		return err
	}
	if len(digestFile) > 0 {
		return os.WriteFile(digestFile, []byte(dgst), 0o640)
	}
	return nil
}

func iterateServices(services map[string]interface{}, proj *types.Project, fn types.ServiceFunc) error {
	return proj.WithServices(nil, func(s types.ServiceConfig) error {
		obj := services[s.Name]
		_, ok := obj.(map[string]interface{})
		if !ok {
			if s.Name == "extensions" {
				fmt.Println("Hacking around https://github.com/compose-spec/compose-go/issues/91")
				return nil
			}
			return fmt.Errorf("Service(%s) has invalid format", s.Name)
		}
		return fn(s)
	})
}

// AppBundle is a pinned compose project together with the app archive made of it
// and the project's supporting files.
type AppBundle struct {
	Compose       []byte
	Archive       []byte
	ContentHashes map[string]string
}

// NewAppBundle archives the current directory, replacing its compose file with the pinned config.
func NewAppBundle(config map[string]interface{}) (*AppBundle, error) {
	pinned, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pinned compose: %w", err)
	}
	fmt.Printf("  |-> pinned content hash: %x\n", sha256.Sum256(pinned))

	buff, appContentHashes, err := createTgz(pinned, "./")
	if err != nil {
		return nil, err
	}
	fmt.Printf("  |-> app archive hash: %x\n", sha256.Sum256(buff))
	return &AppBundle{Compose: pinned, Archive: buff, ContentHashes: appContentHashes}, nil
}

// BuildAppManifest stores the app bundle, its index and the optional layers metadata in the given
// blob store and returns the app manifest referring to them.
func BuildAppManifest(ctx context.Context, blobStore internal.BlobPutStatter, bundle *AppBundle,
	layerManifests []distribution.Descriptor, appLayersMetaData []byte) (*ocischema.DeserializedManifest, error) {
	desc, err := putAppBundle(ctx, blobStore, bundle)
	if err != nil {
		return nil, err
	}
	mb := internal.NewManifestBuilder(blobStore)
	if err := mb.AppendReference(desc); err != nil {
		return nil, err
	}
	if appLayersMetaData != nil {
		d, err := blobStore.Put(ctx, "application/json", appLayersMetaData)
		if err != nil {
			return nil, fmt.Errorf("failed to put App layers meta to the App blob store: %s", err.Error())
		}
		d.Annotations = map[string]string{"layers-meta": "v1"}
		if err := mb.AppendReference(d); err != nil {
			return nil, fmt.Errorf("failed to add App layers meta descriptor to the App manifest: %s", err.Error())
		}
		fmt.Println("  |-> app layers meta: ", d.Digest.String())
	}
	if layerManifests != nil {
		mb.(*internal.ManifestBuilder).SetLayerMetaManifests(layerManifests)
	}

	manifest, err := mb.Build(ctx)
	if err != nil {
		return nil, err
	}
	man, ok := manifest.(*ocischema.DeserializedManifest)
	if !ok {
		return nil, fmt.Errorf("invalid manifest type, expected *ocischema.DeserializedManifest, got: %T", manifest)
	}
	_, b, err := man.Payload()
	if err != nil {
		return nil, err
	}
	fmt.Printf("  |-> manifest size: %d\n", len(b))
	if len(b) >= AppManifestMaxSize {
		return nil, fmt.Errorf("app manifest size (%d) exceeds the maximum size limit (%d)", len(b), AppManifestMaxSize)
	}
	return man, nil
}

func putAppBundle(ctx context.Context, blobStore internal.BlobPutStatter,
	bundle *AppBundle) (distribution.Descriptor, error) {
	desc, err := blobStore.Put(ctx, AppLayerMediaType, bundle.Archive)
	if err != nil {
		return desc, err
	}
	// enforce App's layer media type to make sure it is the same regardless container registry service
	// (some container registries changes the media type specified by a client)
	desc.MediaType = AppLayerMediaType
	fmt.Println("  |-> app blob: ", desc.Digest.String())

	if os.Getenv("APP_BUNDLE_INDEX_OFF") != "1" && len(bundle.ContentHashes) > 0 {
		if d, err := publishAppBundleIndexBlob(ctx, blobStore, bundle.ContentHashes); err == nil {
			desc.Annotations = map[string]string{
				AnnotationKeyAppBundleIndexDigest: d.Digest.String(),
				AnnotationKeyAppBundleIndexSize:   strconv.Itoa(int(d.Size)),
			}
			fmt.Println("  |-> app index: ", d.Digest.String())
		} else {
			fmt.Println("  |-> failed to publish app index blob: ", err.Error())
		}
	}
	return desc, nil
}

func createAndPublishApp(ctx context.Context,
	config map[string]interface{},
	target string, dryRun bool,
	layerManifests []distribution.Descriptor,
	appLayersMetaData []byte) (string, error) {
	bundle, err := NewAppBundle(config)
	if err != nil {
		return "", err
	}

	named, err := reference.ParseNormalizedNamed(target)
	if err != nil {
		return "", err
	}
	tag := "latest"
	if tagged, ok := reference.TagNameOnly(named).(reference.Tagged); ok {
		tag = tagged.Tag()
	}

	regc := internal.NewRegistryClient()
	repo, err := regc.GetRepository(ctx, named)
	if err != nil {
		return "", err
	}

	if dryRun {
		fmt.Println("Pinned compose:")
		fmt.Println(string(bundle.Compose))
		fmt.Println("Skipping publishing for dryrun")
		if err := os.WriteFile("/tmp/compose-bundle.tgz", bundle.Archive, 0755); err != nil {
			return "", err
		}
		return "", nil
	}

	man, err := BuildAppManifest(ctx, repo.Blobs(ctx), bundle, layerManifests, appLayersMetaData)
	if err != nil {
		return "", err
	}
	svc, err := repo.Manifests(ctx, nil)
	if err != nil {
		return "", err
	}

	putOptions := []distribution.ManifestServiceOption{distribution.WithTag(tag)}
	digest, err := svc.Put(ctx, man, putOptions...)
	if err != nil {
		return "", err
	}
	fmt.Println("  |-> manifest: ", digest.String())
	fmt.Println("  |-> uri: ", named.Name()+"@"+digest.String())

	return digest.String(), err
}

func createTgz(composeContent []byte, appDir string) ([]byte, map[string]string, error) {
	if len(composeContent) > AppComposeMaxSize {
		return nil, nil, fmt.Errorf("size of app compose file exceeds the maximum allowed;"+
			" max allowed: %d, size: %d", AppComposeMaxSize, len(composeContent))
	}
	reader, err := archive.TarWithOptions(appDir, &archive.TarOptions{
		Compression:     archive.Uncompressed,
		ExcludePatterns: getIgnores(appDir),
	})
	if err != nil {
		return nil, nil, err
	}

	composeFound := false
	var buf bytes.Buffer
	appContentHashes := make(map[string]string)
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	tr := tar.NewReader(reader)
	var bundleSize int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}

		// reset the file's timestamps, otherwise hashes of the resultant
		// TGZs will differ even if their content is the same
		hdr.ChangeTime = time.Time{}
		hdr.AccessTime = time.Time{}
		hdr.ModTime = time.Time{}
		bundleSize += hdr.Size
		if bundleSize > AppBundleMaxSize {
			return nil, nil, fmt.Errorf("app bundle size exceeds the maximum allowed: %d", AppBundleMaxSize)
		}
		if hdr.Name == "docker-compose.yml" {
			composeFound = true
			hdr.Size = int64(len(composeContent))
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, nil, fmt.Errorf("Unable to add docker-compose.yml header archive: %s", err)
			}
			if _, err := tw.Write(composeContent); err != nil {
				return nil, nil, fmt.Errorf("Unable to add docker-compose.yml to archive: %s", err)
			}
			h := sha256.Sum256(composeContent)
			appContentHashes[hdr.Name] = "sha256:" + hex.EncodeToString(h[:])
		} else {
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, nil, fmt.Errorf("Unable to add %s header archive: %s", hdr.Name, err)
			}
			var r io.Reader = tr
			var h hash.Hash
			if !hdr.FileInfo().IsDir() {
				if hdr.Size > AppBundleFileMaxSize {
					return nil, nil, fmt.Errorf("size of app bundle file exceeds the maximum allowed;"+
						" file: %s, max allowed: %d, size: %d", hdr.Name, AppBundleFileMaxSize, len(composeContent))
				}
				h = sha256.New()
				r = io.TeeReader(tr, h)
			}
			if _, err := io.Copy(tw, r); err != nil {
				return nil, nil, fmt.Errorf("Unable to add %s archive: %s", hdr.Name, err)
			}
			if h != nil {
				appContentHashes[hdr.Name] = "sha256:" + hex.EncodeToString(h.Sum(nil))
			}
		}
	}

	if !composeFound {
		return nil, nil, errors.New("A .composeappignores rule is discarding docker-compose.yml")
	}

	tw.Close()
	gzw.Close()
	return buf.Bytes(), appContentHashes, nil
}

func getIgnores(appDir string) []string {
	file, err := os.Open(filepath.Join(appDir, ".composeappignores"))
	if err != nil {
		return []string{}
	}
	ignores, _ := ignorefile.ReadAll(file)
	file.Close()
	if ignores != nil {
		ignores = append(ignores, ".composeappignores")
	}
	return ignores
}

func pinServiceImages(ctx context.Context,
	services map[string]interface{},
	proj *types.Project,
	resolver ImageResolver) error {
	return iterateServices(services, proj, func(s types.ServiceConfig) error {
		svc := services[s.Name].(map[string]interface{})
		if s.Build != nil {
			fmt.Printf("Removing service(%s) 'build' stanza\n", s.Name)
			delete(svc, "build")
		}
		pinned, err := resolver.ResolveImage(ctx, s)
		if err != nil {
			return err
		}
		fmt.Println("\n  |-> ", pinned)
		svc["image"] = pinned
		return nil
	})
}

func (r *registryImageResolver) ResolveImage(ctx context.Context, s types.ServiceConfig) (string, error) {
	image := s.Image
	if len(image) == 0 {
		return "", fmt.Errorf("Service(%s) missing 'image' attribute", s.Name)
	}
	fmt.Printf("Pinning %s(%s)\n", s.Name, image)
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", err
	}
	repo, err := r.regc.GetRepository(ctx, named)
	if err != nil {
		return "", err
	}

	var digest digest.Digest
	switch v := named.(type) {
	case reference.Tagged:
		desc, err := repo.Tags(ctx).Get(ctx, v.Tag())
		if err != nil {
			return "", fmt.Errorf("Unable to find image reference(%s): %s", image, err)
		}
		digest = desc.Digest
	case reference.Digested:
		digest = v.Digest()
	default:
		var ok bool
		if digest, ok = r.pinnedImages[named.Name()]; !ok {
			return "", fmt.Errorf("Invalid reference type for %s: %T. Images must be pinned to a `:<tag>` or `@sha256:<hash>`", named, named)
		}
	}

	mansvc, err := repo.Manifests(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("Unable to get image manifests(%s): %s", image, err)
	}
	man, err := mansvc.Get(ctx, digest)
	if err != nil {
		return "", fmt.Errorf("Unable to find image manifest(%s): %s", image, err)
	}
	if err := printManifestPlatforms(man); err != nil {
		return "", err
	}
	// TODO - we should find the intersection of platforms so
	// that we can denote the platforms this app can run on
	return reference.Domain(named) + "/" + reference.Path(named) + "@" + digest.String(), nil
}

func printManifestPlatforms(man distribution.Manifest) error {
	switch mani := man.(type) {
	case *manifestlist.DeserializedManifestList:
		fmt.Printf("  | ")
		for i, m := range mani.Manifests {
			if i != 0 {
				fmt.Printf(", ")
			}
			fmt.Printf(m.Platform.Architecture)
			if m.Platform.Architecture == "arm" {
				fmt.Printf(m.Platform.Variant)
			}
		}
	case *schema2.DeserializedManifest:
		break
	default:
		return fmt.Errorf("Unexpected manifest: %v", mani)
	}
	return nil
}

func pinServiceConfigs(services map[string]interface{}, proj *types.Project) error {
	return iterateServices(services, proj, func(s types.ServiceConfig) error {
		obj := services[s.Name]
		svc := obj.(map[string]interface{})

		marshalled, err := yaml.Marshal(svc)
		if err != nil {
			return err
		}

		srvh := sha256.Sum256(marshalled)
		fmt.Printf("   |-> %s : %x\n", s.Name, srvh)
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}
		s.Labels["io.compose-spec.config-hash"] = fmt.Sprintf("%x", srvh)
		svc["labels"] = s.Labels
		return nil
	})
}

func publishAppBundleIndexBlob(
	ctx context.Context,
	blobStore internal.BlobPutStatter,
	appContentHashes map[string]string) (desc distribution.Descriptor, err error) {
	var b []byte
	if b, err = json.Marshal(appContentHashes); err == nil {
		desc, err = blobStore.Put(ctx, "application/json", b)
	}
	return
}
