//go:build publish

package composectl

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/docker/distribution/reference"
	v1 "github.com/foundriesio/composeapp/pkg/compose/v1"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
)

type (
	buildOptions struct {
		ComposeFile     string
		DigestFile      string
		Builder         string
		PinnedImageURIs []string
	}
)

func init() {
	buildCmd := &cobra.Command{
		Use:   "build <ref> <comma,separated,arch,list>",
		Short: "Build app images and the app into the local app store without pushing to a registry",
		Long: `Build the images of the app services that have a "build" stanza for each of the given
architectures, then store the app, its built images, and the images it refers to remotely in the
app store (see --store). The resulting store directory can be used offline, e.g. as the source of
"pull --source-store-path" or copied onto a device.

<ref> names the app, e.g. hub.example.com/acme/myapp:1.0; nothing is pushed to it. Services
without an "image" are given one made of the app repository and the service name.

Building requires the docker buildx plugin. A buildx builder using the docker-container driver
is created if the one given by --builder does not exist.`,
		Args: cobra.ExactArgs(2),
	}
	opts := buildOptions{}
	buildCmd.Flags().StringVarP(&opts.ComposeFile, "file", "f", "docker-compose.yml", "A path to a compose project file")
	buildCmd.Flags().StringVarP(&opts.DigestFile, "digest-file", "d", "", "A file to store the built app sha256 digest to")
	buildCmd.Flags().StringVar(&opts.Builder, "builder", "composectl", "The buildx builder to build images with")
	buildCmd.Flags().StringSliceVar(&opts.PinnedImageURIs, "pinned-images", nil,
		"A list of app images referred through digest URIs to pin app to")

	buildCmd.Run = func(cmd *cobra.Command, args []string) {
		pinnedImages, err := parsePinnedImages(opts.PinnedImageURIs)
		DieNotNil(err)
		buildApp(cmd, args[0], strings.Split(args[1], ","), pinnedImages, &opts)
	}
	rootCmd.AddCommand(buildCmd)
}

func buildApp(cmd *cobra.Command, appRef string, archs []string, pinnedImages map[string]digest.Digest,
	opts *buildOptions) {
	workDir, err := os.MkdirTemp("", "composectl-build-")
	DieNotNil(err, "failed to create build directory")
	AddLastWill(func() { os.RemoveAll(workDir) })
	defer os.RemoveAll(workDir)

	fmt.Printf("Building %s to %s\n", appRef, config.StoreRoot)
	uri, err := v1.DoBuild(cmd.Context(), config, &v1.BuildOptions{
		AppRef:       appRef,
		ComposeFile:  opts.ComposeFile,
		Archs:        archs,
		Builder:      opts.Builder,
		WorkDir:      workDir,
		PinnedImages: pinnedImages,
	})
	DieNotNil(err, "failed to build app")
	if len(opts.DigestFile) > 0 {
		dgst := uri[strings.LastIndex(uri, "@")+1:]
		DieNotNil(os.WriteFile(opts.DigestFile, []byte(dgst), 0o640), "failed to write digest file")
	}
	fmt.Println("App built: " + uri)
}

func parsePinnedImages(uris []string) (map[string]digest.Digest, error) {
	pinnedImages := map[string]digest.Digest{}
	for _, uri := range uris {
		named, err := reference.ParseNormalizedNamed(uri)
		if err != nil {
			return nil, fmt.Errorf("invalid image URI specified in `pinned-images`; uri: %s, err: %w", uri, err)
		}
		digested, ok := named.(reference.Digested)
		if !ok {
			return nil, errors.New("Image URI specified in `pinned-images` is not digested: " + uri)
		}
		pinnedImages[named.Name()] = digested.Digest()
	}
	return pinnedImages, nil
}
