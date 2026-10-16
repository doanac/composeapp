//go:build publish

package v1

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/types"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/distribution/reference"
	"github.com/foundriesio/composeapp/internal"
	"github.com/foundriesio/composeapp/pkg/compose"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type (
	// BuildOptions configures building an app straight into a local app store.
	BuildOptions struct {
		// AppRef is the app's reference without a digest, e.g. hub.example.com/acme/myapp:1.0;
		// it names the app and its image repositories, nothing is pushed to it.
		AppRef      string
		ComposeFile string
		Archs       []string
		Builder     string
		// WorkDir holds the OCI layouts exported by buildx; it is created if missing
		WorkDir      string
		PinnedImages map[string]digest.Digest
	}

	// localImageResolver pins images of services that have a `build` stanza to the digests of the
	// images just built, and falls back to the registry for all other services.
	localImageResolver struct {
		built    map[string]string
		registry ImageResolver
	}
)

// DoBuild builds the app service images for all requested architectures, pins the app to them and to
// the images it refers to remotely, and stores the app with all of its blobs in the app store of cfg.
// It returns the URI of the app, including its manifest digest.
func DoBuild(ctx context.Context, cfg *compose.Config, opts *BuildOptions) (string, error) {
	named, err := parseBuildAppRef(opts.AppRef)
	if err != nil {
		return "", err
	}
	appName := filepath.Base(reference.Path(named))
	platformSpecs, err := toPlatforms(opts.Archs)
	if err != nil {
		return "", err
	}
	bs, err := compose.NewLocalBlobStore(cfg.StoreRoot)
	if err != nil {
		return "", err
	}

	fmt.Println("= Building service images...")
	built, err := buildServiceImages(ctx, cfg, bs, named, appName, platformSpecs, opts)
	if err != nil {
		return "", err
	}
	resolver := &localImageResolver{built: built, registry: NewRegistryImageResolver(opts.PinnedImages)}
	config, err := LoadAndPinApp(ctx, appName, opts.ComposeFile, resolver)
	if err != nil {
		return "", err
	}

	fmt.Println("= Storing app...")
	appURI, err := storeApp(ctx, bs, named, config)
	if err != nil {
		return "", err
	}
	fmt.Println("  |-> uri: ", appURI)
	if err := fetchAndFinalize(ctx, cfg, appURI, platformSpecs); err != nil {
		return "", err
	}
	return appURI, nil
}

func parseBuildAppRef(ref string) (reference.Named, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid app reference %q: %w", ref, err)
	}
	if _, ok := named.(reference.Digested); ok {
		return nil, fmt.Errorf("invalid app reference %q: cannot be a reference with digest", ref)
	}
	return named, nil
}

func toPlatforms(archs []string) ([]string, error) {
	if len(archs) == 0 {
		return nil, errors.New("at least one architecture must be specified")
	}
	var out []string
	seen := map[string]bool{}
	for _, a := range archs {
		p, err := internal.ArchToPlatform(strings.TrimSpace(a))
		if err != nil {
			return nil, err
		}
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out, nil
}

// buildServiceImages builds every service with a `build` stanza and ingests the resulting images
// into the app store. It returns the pinned image URI of each built service.
func buildServiceImages(ctx context.Context, cfg *compose.Config, bs *compose.LocalBlobStore, appRef reference.Named,
	appName string, platformSpecs []string, opts *BuildOptions) (map[string]string, error) {
	b, err := os.ReadFile(opts.ComposeFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read compose file: %w", err)
	}
	proj, err := loadProj(ctx, appName, opts.ComposeFile, b)
	if err != nil {
		return nil, fmt.Errorf("failed to load compose project: %w", err)
	}
	outputs := map[string]string{}
	for _, s := range proj.Services {
		if s.Build != nil {
			outputs[s.Name] = filepath.Join(opts.WorkDir, s.Name)
		}
	}
	built := map[string]string{}
	if len(outputs) == 0 {
		fmt.Println("  |-> no services to build")
		return built, nil
	}
	if err := runBake(ctx, cfg, opts, platformSpecs, outputs); err != nil {
		return nil, err
	}
	for _, s := range proj.Services {
		if s.Build == nil {
			continue
		}
		uri, err := ingestBuiltImage(ctx, bs, appRef, s, outputs[s.Name], platformSpecs)
		if err != nil {
			return nil, fmt.Errorf("failed to store image of service %s: %w", s.Name, err)
		}
		built[s.Name] = uri
	}
	return built, nil
}

func runBake(ctx context.Context, cfg *compose.Config, opts *BuildOptions, platformSpecs []string,
	outputs map[string]string) error {
	for _, dir := range outputs {
		// buildx refuses to export into a non-empty directory
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("failed to clean build output directory %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return fmt.Errorf("failed to create build directory %s: %w", opts.WorkDir, err)
	}
	if err := internal.EnsureBuilder(ctx, opts.Builder, cfg.DockerHost); err != nil {
		return err
	}
	return internal.RunBake(ctx, &internal.BakeOptions{
		ComposeFile: opts.ComposeFile,
		Builder:     opts.Builder,
		Platforms:   platformSpecs,
		Outputs:     outputs,
		OutputRoot:  opts.WorkDir,
		DockerHost:  cfg.DockerHost,
	})
}

func ingestBuiltImage(ctx context.Context, bs *compose.LocalBlobStore, appRef reference.Named, s types.ServiceConfig,
	layoutDir string, platformSpecs []string) (string, error) {
	root, err := bs.IngestOCILayout(ctx, layoutDir)
	if err != nil {
		return "", err
	}
	if err := checkBuiltPlatforms(ctx, bs, root, platformSpecs); err != nil {
		return "", err
	}
	name, err := builtImageName(appRef, s)
	if err != nil {
		return "", err
	}
	uri := name + "@" + root.Digest.String()
	fmt.Printf("  |-> %s: %s\n", s.Name, uri)
	return uri, nil
}

// checkBuiltPlatforms makes sure an image was built for every requested platform; a single
// platform build may be exported as a plain manifest rather than an index.
func checkBuiltPlatforms(ctx context.Context, bs *compose.LocalBlobStore, root *ocispec.Descriptor,
	platformSpecs []string) error {
	if len(platformSpecs) == 1 && root.MediaType == ocispec.MediaTypeImageManifest {
		return nil
	}
	found, err := bs.IndexPlatforms(ctx, root)
	if err != nil {
		return err
	}
	for _, spec := range platformSpecs {
		p, err := platforms.Parse(spec)
		if err != nil {
			return fmt.Errorf("invalid platform %s: %w", spec, err)
		}
		if !platforms.Any(found...).Match(p) {
			return fmt.Errorf("image was not built for platform %s", spec)
		}
	}
	return nil
}

// builtImageName returns the repository a built service image is referred to by. A service image name
// is used as is, otherwise one is made up of the app's repository and the service name.
func builtImageName(appRef reference.Named, s types.ServiceConfig) (string, error) {
	if len(s.Image) > 0 {
		named, err := reference.ParseNormalizedNamed(s.Image)
		if err != nil {
			return "", fmt.Errorf("invalid image name %q: %w", s.Image, err)
		}
		return named.Name(), nil
	}
	name := appRef.Name() + "-" + strings.ToLower(s.Name)
	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return "", fmt.Errorf("failed to make up image name for service %s: %w", s.Name, err)
	}
	return named.Name(), nil
}

func (r *localImageResolver) ResolveImage(ctx context.Context, s types.ServiceConfig) (string, error) {
	if uri, ok := r.built[s.Name]; ok {
		fmt.Printf("Pinning %s to the built image\n", s.Name)
		return uri, nil
	}
	return r.registry.ResolveImage(ctx, s)
}

func storeApp(ctx context.Context, bs *compose.LocalBlobStore, appRef reference.Named,
	config map[string]interface{}) (string, error) {
	bundle, err := NewAppBundle(config)
	if err != nil {
		return "", err
	}
	man, err := BuildAppManifest(ctx, bs, bundle, nil, nil)
	if err != nil {
		return "", err
	}
	_, b, err := man.Payload()
	if err != nil {
		return "", fmt.Errorf("failed to serialize app manifest: %w", err)
	}
	desc, err := bs.Put(ctx, AppManifestMediaType, b)
	if err != nil {
		return "", err
	}
	fmt.Println("  |-> manifest: ", desc.Digest.String())
	return appRef.Name() + "@" + desc.Digest.String(), nil
}

// fetchAndFinalize pulls the blobs of remotely referred images for all platforms, then registers the app
// in the store in the same way `pull` does, so it can be checked, installed, or copied offline.
func fetchAndFinalize(ctx context.Context, cfg *compose.Config, appURI string, platformSpecs []string) error {
	store, err := NewAppStore(cfg.StoreRoot, cfg.Platform)
	if err != nil {
		return fmt.Errorf("failed to open app store: %w", err)
	}
	var specs []ocispec.Platform
	for _, spec := range platformSpecs {
		p, err := platforms.Parse(spec)
		if err != nil {
			return fmt.Errorf("invalid platform %s: %w", spec, err)
		}
		specs = append(specs, p)
	}
	matcher := platforms.Any(specs...)
	// the app and the built images are in the store already, the remote images are not yet
	provider := compose.NewFallbackBlobProvider(store, compose.NewRemoteBlobProviderFromConfig(cfg))
	app, err := cfg.AppLoader.LoadAppTree(ctx, provider, matcher, appURI)
	if err != nil {
		return fmt.Errorf("failed to load app: %w", err)
	}
	status, err := compose.CheckAppsFetchStatus(ctx, cfg, store, []compose.App{app}, false)
	if err != nil {
		return fmt.Errorf("failed to check app blobs: %w", err)
	}
	fmt.Printf("= Fetching %d blobs of remote images...\n", len(status.MissingBlobs))
	if len(status.MissingBlobs) > 0 {
		if err := compose.FetchBlobs(ctx, cfg, status.MissingBlobs); err != nil {
			return fmt.Errorf("failed to fetch app blobs: %w", err)
		}
	}
	if err := MakeAkliteHappy(ctx, store, app, matcher); err != nil {
		return fmt.Errorf("failed to register app in store: %w", err)
	}
	return nil
}
