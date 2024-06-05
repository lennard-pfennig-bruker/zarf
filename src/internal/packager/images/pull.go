// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2021-Present The Zarf Authors

// Package images provides functions for building and pushing images.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/defenseunicorns/pkg/helpers"
	"github.com/defenseunicorns/zarf/src/config"
	"github.com/defenseunicorns/zarf/src/pkg/layout"
	"github.com/defenseunicorns/zarf/src/pkg/message"
	"github.com/defenseunicorns/zarf/src/pkg/transform"
	"github.com/defenseunicorns/zarf/src/pkg/utils"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/cache"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	clayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

// Pull pulls all of the images from the given config.
func Pull(ctx context.Context, cfg PullConfig) (map[transform.Image]v1.Image, error) {
	var longer string
	imageCount := len(cfg.ImageList)
	// Give some additional user feedback on larger image sets
	if imageCount > 15 {
		longer = "This step may take a couple of minutes to complete."
	} else if imageCount > 5 {
		longer = "This step may take several seconds to complete."
	}

	if err := helpers.CreateDirectory(cfg.DestinationDirectory, helpers.ReadExecuteAllWriteUser); err != nil {
		return nil, fmt.Errorf("failed to create image path %s: %w", cfg.DestinationDirectory, err)
	}

	cranePath, err := clayout.Write(cfg.DestinationDirectory, empty.Index)
	if err != nil {
		return nil, err
	}

	spinner := message.NewProgressSpinner("Fetching info for %d images. %s", imageCount, longer)
	defer spinner.Stop()

	logs.Warn.SetOutput(&message.DebugWriter{})
	logs.Progress.SetOutput(&message.DebugWriter{})

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(10)

	var shaLock sync.Mutex
	shas := map[string]bool{}
	opts := CommonOpts(cfg.Arch)

	fetched := map[transform.Image]v1.Image{}

	var counter, totalBytes atomic.Int64

	for _, refInfo := range cfg.ImageList {
		refInfo := refInfo
		eg.Go(func() error {
			idx := counter.Add(1)
			spinner.Updatef("Fetching image info (%d of %d)", idx, imageCount)

			ref := refInfo.Reference
			for k, v := range cfg.RegistryOverrides {
				if strings.HasPrefix(refInfo.Reference, k) {
					ref = strings.Replace(refInfo.Reference, k, v, 1)
				}
			}

			var img v1.Image
			var desc *remote.Descriptor

			// load from local fs if it's a tarball
			if strings.HasSuffix(ref, ".tar") || strings.HasSuffix(ref, ".tar.gz") || strings.HasSuffix(ref, ".tgz") {
				img, err = crane.Load(ref, opts...)
				if err != nil {
					return fmt.Errorf("unable to load %s: %w", refInfo.Reference, err)
				}
			} else {
				reference, err := name.ParseReference(ref)
				if err != nil {
					return fmt.Errorf("failed to parse reference: %w", err)
				}
				desc, err = crane.Get(ref, opts...)
				if err != nil {
					if strings.Contains(err.Error(), "unexpected status code 429 Too Many Requests") {
						return fmt.Errorf("rate limited by registry: %w", err)
					}

					message.Warnf("Falling back to local 'docker', failed to find the manifest on a remote: %s", err.Error())

					// Attempt to connect to the local docker daemon.
					cli, err := client.NewClientWithOpts(client.FromEnv)
					if err != nil {
						return fmt.Errorf("docker not available: %w", err)
					}
					cli.NegotiateAPIVersion(ectx)

					// Inspect the image to get the size.
					rawImg, _, err := cli.ImageInspectWithRaw(ectx, ref)
					if err != nil {
						return err
					}

					// Warn the user if the image is large.
					if rawImg.Size > 750*1000*1000 {
						message.Warnf("%s is %s and may take a very long time to load via docker. "+
							"See https://docs.zarf.dev/faq for suggestions on how to improve large local image loading operations.",
							ref, utils.ByteFormat(float64(rawImg.Size), 2))
					}

					// Use unbuffered opener to avoid OOM Kill issues https://github.com/defenseunicorns/zarf/issues/1214.
					// This will also take forever to load large images.
					img, err = daemon.Image(reference, daemon.WithUnbufferedOpener())
					if err != nil {
						return fmt.Errorf("failed to load from docker daemon: %w", err)
					}
				} else {
					img, err = crane.Pull(ref, opts...)
					if err != nil {
						return fmt.Errorf("unable to pull image %s: %w", refInfo.Reference, err)
					}
				}
			}

			if refInfo.Digest != "" && desc != nil && types.MediaType(desc.MediaType).IsIndex() {
				message.Warn("Zarf does not currently support direct consumption of OCI image indexes or Docker manifest lists")

				var idx v1.IndexManifest
				if err := json.Unmarshal(desc.Manifest, &idx); err != nil {
					return fmt.Errorf("unable to unmarshal index manifest: %w", err)
				}
				lines := []string{"The following images are available in the index:"}
				name := refInfo.Name
				if refInfo.Tag != "" {
					name += ":" + refInfo.Tag
				}
				for _, desc := range idx.Manifests {
					lines = append(lines, fmt.Sprintf("\n(%s) %s@%s", desc.Platform, name, desc.Digest))
				}
				message.Warn(strings.Join(lines, "\n"))
				return fmt.Errorf("%s resolved to an index, please select a specific platform to use", refInfo.Reference)
			}

			img = cache.Image(img, cache.NewFilesystemCache(cfg.CacheDirectory))

			manifest, err := img.Manifest()
			if err != nil {
				return fmt.Errorf("unable to get manifest for %s: %w", refInfo.Reference, err)
			}
			totalBytes.Add(manifest.Config.Size)

			layers, err := img.Layers()
			if err != nil {
				return fmt.Errorf("unable to get layers for %s: %w", refInfo.Reference, err)
			}

			shaLock.Lock()
			defer shaLock.Unlock()
			for _, layer := range layers {
				digest, err := layer.Digest()
				if err != nil {
					return fmt.Errorf("unable to get digest for image layer: %w", err)
				}

				if _, ok := shas[digest.Hex]; !ok {
					shas[digest.Hex] = true
					size, err := layer.Size()
					if err != nil {
						return fmt.Errorf("unable to get size for image layer: %w", err)
					}
					totalBytes.Add(size)
				}
			}

			if img == nil {
				return fmt.Errorf("failed to fetch image %s", refInfo.Reference)
			}

			fetched[refInfo] = img

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	spinner.Successf("Fetched info for %d images", imageCount)

	doneSaving := make(chan error)
	updateText := fmt.Sprintf("Pulling %d images", imageCount)
	go utils.RenderProgressBarForLocalDirWrite(cfg.DestinationDirectory, totalBytes.Load(), doneSaving, updateText, updateText)

	toPull := maps.Clone(fetched)

	sc := func() error {
		saved, err := SaveConcurrent(ctx, cranePath, toPull)
		for k := range saved {
			delete(toPull, k)
		}
		return err
	}

	ss := func() error {
		saved, err := SaveSequential(ctx, cranePath, toPull)
		for k := range saved {
			delete(toPull, k)
		}
		return err
	}

	if err := helpers.Retry(sc, 2, 5*time.Second, message.Warnf); err != nil {
		message.Warnf("Failed to save images in parallel, falling back to sequential save: %s", err.Error())

		if err := helpers.Retry(ss, 2, 5*time.Second, message.Warnf); err != nil {
			return nil, err
		}
	}

	// Send a signal to the progress bar that we're done and wait for the thread to finish
	doneSaving <- nil
	<-doneSaving

	// This fixes an issue on amd64, when pulling images from the local docker daemon,
	// while using the docker containerd runtime. Crane incorrectly names the blob of the docker image config
	// to a sha that does not match the contents
	// https://github.com/defenseunicorns/zarf/issues/2584
	// This is a band aid fix while we wait for crane and the docker to create the permanent fix
	blobDirectory := filepath.Join(cfg.DestinationDirectory, "images", "blobs", "sha256")
	err = filepath.Walk(blobDirectory, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		hash, err := helpers.GetSHA256Hash(file)
		if err != nil {
			return err
		}

		return os.Rename(path, hash)
	})
	if err != nil {
		return nil, err
	}

	return fetched, nil
}

// CleanupInProgressLayers removes incomplete layers from the cache.
func CleanupInProgressLayers(ctx context.Context, img v1.Image) error {
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	eg, _ := errgroup.WithContext(ctx)
	for _, layer := range layers {
		layer := layer
		eg.Go(func() error {
			digest, err := layer.Digest()
			if err != nil {
				return err
			}
			size, err := layer.Size()
			if err != nil {
				return err
			}
			cacheDir := filepath.Join(config.GetAbsCachePath(), layout.ImagesDir)
			location := filepath.Join(cacheDir, digest.String())
			info, err := os.Stat(location)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if info.Size() != size {
				if err := os.Remove(location); err != nil {
					return fmt.Errorf("failed to remove incomplete layer %s: %w", digest.Hex, err)
				}
			}
			return nil
		})
	}
	return eg.Wait()
}

// SaveSequential saves images sequentially.
func SaveSequential(ctx context.Context, cl clayout.Path, m map[transform.Image]v1.Image) (map[transform.Image]v1.Image, error) {
	saved := map[transform.Image]v1.Image{}
	for info, img := range m {
		annotations := map[string]string{
			ocispec.AnnotationBaseImageName: info.Reference,
		}
		if err := cl.AppendImage(img, clayout.WithAnnotations(annotations)); err != nil {
			if err := CleanupInProgressLayers(ctx, img); err != nil {
				message.WarnErr(err, "failed to clean up in-progress layers, please run `zarf tools clear-cache`")
			}
			return saved, err
		}
		saved[info] = img
	}
	return saved, nil
}

// SaveConcurrent saves images in a concurrent, bounded manner.
func SaveConcurrent(ctx context.Context, cl clayout.Path, m map[transform.Image]v1.Image) (map[transform.Image]v1.Image, error) {
	saved := map[transform.Image]v1.Image{}

	var mu sync.Mutex

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(10)

	for info, img := range m {
		info, img := info, img
		eg.Go(func() error {
			desc, err := partial.Descriptor(img)
			if err != nil {
				return err
			}

			if err := cl.WriteImage(img); err != nil {
				if err := CleanupInProgressLayers(ectx, img); err != nil {
					message.WarnErr(err, "failed to clean up in-progress layers, please run `zarf tools clear-cache`")
				}
				return err
			}

			mu.Lock()
			defer mu.Unlock()
			annotations := map[string]string{
				ocispec.AnnotationBaseImageName: info.Reference,
			}
			desc.Annotations = annotations
			if err := cl.AppendDescriptor(*desc); err != nil {
				return err
			}

			saved[info] = img
			return nil
		})
	}

	return saved, eg.Wait()
}
