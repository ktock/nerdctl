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

package container

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"testing"

	"gotest.tools/v3/assert"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/nerdctl/mod/tigron/expect"
	"github.com/containerd/nerdctl/mod/tigron/require"
	"github.com/containerd/nerdctl/mod/tigron/test"
	"github.com/containerd/nerdctl/mod/tigron/tig"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	imagespecversioned "github.com/opencontainers/image-spec/specs-go"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/nerdctl/v2/pkg/rootlessutil"
	"github.com/containerd/nerdctl/v2/pkg/testutil"
	"github.com/containerd/nerdctl/v2/pkg/testutil/nerdtest"
	"github.com/containerd/nerdctl/v2/pkg/testutil/nerdtest/registry"
)

func TestRunStargz(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Require = require.All(
		nerdtest.Stargz,
		require.Amd64,
		require.Not(nerdtest.Docker),
	)

	testCase.Command = test.Command("--snapshotter=stargz", "run", "--quiet", "--rm", testutil.FedoraESGZImage, "ls", "/.stargz-snapshotter")

	testCase.Expected = test.Expects(0, nil, nil)

	testCase.Run(t)
}

// createInvalidESGZImage creates an eStargz image from the first layer of srcRef with an extra file added to
// the layer, while keeping the original image config. As the diffID in the config doesn't match the modified
// layer, the resulting image has an invalid ChainID. The image is stored in containerd as newRef.
func createInvalidESGZImage(ctx context.Context, t tig.T, client *containerd.Client, srcRef, newRef string) {
	cs := client.ContentStore()
	srcImg, err := client.GetImage(ctx, srcRef)
	assert.NilError(t, err)
	srcManifest, err := images.Manifest(ctx, cs, srcImg.Target(), platforms.Default())
	assert.NilError(t, err)
	layer, err := content.ReadBlob(ctx, cs, srcManifest.Layers[0])
	assert.NilError(t, err)

	var modified bytes.Buffer
	tw := tar.NewWriter(&modified)
	gz, err := gzip.NewReader(bytes.NewReader(layer))
	assert.NilError(t, err)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		assert.NilError(t, err)
		assert.NilError(t, tw.WriteHeader(h))
		_, err = io.Copy(tw, tr)
		assert.NilError(t, err)
	}
	contents := "MODIFIED"
	assert.NilError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "/modified.txt", Size: int64(len(contents))}))
	_, err = tw.Write([]byte(contents))
	assert.NilError(t, err)
	assert.NilError(t, tw.Close())

	modifiedData := modified.Bytes()
	esgz, err := estargz.Build(io.NewSectionReader(bytes.NewReader(modifiedData), 0, int64(len(modifiedData))))
	assert.NilError(t, err)
	defer esgz.Close()
	layerData, err := io.ReadAll(esgz)
	assert.NilError(t, err)
	layerDesc := imagespec.Descriptor{
		MediaType:   imagespec.MediaTypeImageLayerGzip,
		Digest:      digest.FromBytes(layerData),
		Size:        int64(len(layerData)),
		Annotations: map[string]string{estargz.TOCJSONDigestAnnotation: esgz.TOCDigest().String()},
	}
	assert.NilError(t, content.WriteBlob(ctx, cs, "layer-invalid", bytes.NewReader(layerData), layerDesc))

	srcConfig, err := images.Config(ctx, cs, srcImg.Target(), platforms.Default())
	assert.NilError(t, err)
	manifest := &imagespec.Manifest{
		Versioned: imagespecversioned.Versioned{SchemaVersion: 2},
		MediaType: imagespec.MediaTypeImageManifest,
		Config:    srcConfig,
		Layers:    []imagespec.Descriptor{layerDesc},
	}
	manifestBlob, err := json.Marshal(manifest)
	assert.NilError(t, err)
	manifestDesc := imagespec.Descriptor{
		MediaType: imagespec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestBlob),
		Size:      int64(len(manifestBlob)),
	}
	labels := map[string]string{}
	for i, d := range []imagespec.Descriptor{layerDesc, srcConfig} {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.%d", i)] = d.Digest.String()
	}
	assert.NilError(t, content.WriteBlob(ctx, cs, "manifest-invalid", bytes.NewReader(manifestBlob), manifestDesc, content.WithLabels(labels)))
	_, err = client.ImageService().Create(ctx, images.Image{Name: newRef, Target: manifestDesc})
	assert.NilError(t, err)
}

func newTestContainerdClient(helpers test.Helpers) *containerd.Client {
	addr := defaults.DefaultAddress
	if rootlessutil.IsRootless() {
		stateDir, err := rootlessutil.RootlessKitStateDir()
		assert.NilError(helpers.T(), err)
		childPid, err := rootlessutil.RootlessKitChildPid(stateDir)
		assert.NilError(helpers.T(), err)
		addr = filepath.Join("/proc", fmt.Sprintf("%d", childPid), "root", defaults.DefaultAddress)
	}
	client, err := containerd.New(addr, containerd.WithDefaultNamespace(string(helpers.Read(nerdtest.Namespace))))
	assert.NilError(helpers.T(), err)
	return client
}

// diffIDOfLayer returns the digest of the decompressed content of the layer blob.
func diffIDOfLayer(ctx context.Context, cs content.Store, desc imagespec.Descriptor) (digest.Digest, error) {
	ra, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		return "", err
	}
	defer ra.Close()
	gz, err := gzip.NewReader(content.NewReader(ra))
	if err != nil {
		return "", err
	}
	defer gz.Close()
	dgstr := digest.Canonical.Digester()
	if _, err := io.Copy(dgstr.Hash(), gz); err != nil {
		return "", err
	}
	return dgstr.Digest(), nil
}

// dumpImage logs what the stargz verifier looks at for the image: the layers (with the TOC digest annotation and
// the actual diffID of the blob), and the diffIDs / ChainIDs recorded in the image config.
func dumpImage(ctx context.Context, t tig.T, client *containerd.Client, ref string) {
	t.Helper()
	cs := client.ContentStore()
	img, err := client.GetImage(ctx, ref)
	if err != nil {
		t.Log(fmt.Sprintf("[dump] image %s: %v", ref, err))
		return
	}
	manifest, err := images.Manifest(ctx, cs, img.Target(), platforms.Default())
	assert.NilError(t, err)
	var config imagespec.Image
	cfgBlob, err := content.ReadBlob(ctx, cs, manifest.Config)
	assert.NilError(t, err)
	assert.NilError(t, json.Unmarshal(cfgBlob, &config))
	var out bytes.Buffer
	fmt.Fprintf(&out, "[dump] image %s (target=%s %s)\n", ref, img.Target().MediaType, img.Target().Digest)
	fmt.Fprintf(&out, "  config=%s\n", manifest.Config.Digest)
	for i, l := range manifest.Layers {
		actual, err := diffIDOfLayer(ctx, cs, l)
		fmt.Fprintf(&out, "  layer[%d] digest=%s size=%d mediatype=%s\n", i, l.Digest, l.Size, l.MediaType)
		fmt.Fprintf(&out, "    toc-digest annotation=%q\n", l.Annotations[estargz.TOCJSONDigestAnnotation])
		fmt.Fprintf(&out, "    actual diffID of blob=%s (err=%v)\n", actual, err)
		if i < len(config.RootFS.DiffIDs) {
			fmt.Fprintf(&out, "    diffID in config    =%s (match=%v)\n", config.RootFS.DiffIDs[i], config.RootFS.DiffIDs[i] == actual)
		}
	}
	chainIDs := identity.ChainIDs(append([]digest.Digest{}, config.RootFS.DiffIDs...))
	fmt.Fprintf(&out, "  chainIDs from config=%v\n", chainIDs)
	t.Log(out.String())
}

// dumpStargzSnapshots logs the snapshots known to the stargz snapshotter, with their labels.
func dumpStargzSnapshots(ctx context.Context, t tig.T, client *containerd.Client, title string) {
	t.Helper()
	var out bytes.Buffer
	fmt.Fprintf(&out, "[dump] stargz snapshots (%s)\n", title)
	err := client.SnapshotService("stargz").Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		fmt.Fprintf(&out, "  name=%s kind=%s parent=%s\n", info.Name, info.Kind, info.Parent)
		keys := make([]string, 0, len(info.Labels))
		for k := range info.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&out, "    %s=%s\n", k, info.Labels[k])
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(&out, "  walk error: %v\n", err)
	}
	t.Log(out.String())
}

func TestPullStargzInvalidChainID(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Require = require.All(
		nerdtest.Stargz,
		nerdtest.Registry,
		require.Amd64,
		require.Not(nerdtest.Docker),
	)

	var reg *registry.Server

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		reg = nerdtest.RegistryWithNoAuth(data, helpers, 0, false)
		reg.Setup(data, helpers)
		base := fmt.Sprintf("127.0.0.1:%d/alpine", reg.Port)
		esgz := base + ":esgz"
		invalid := base + ":esgz-invalid"

		// A valid eStargz image
		helpers.Ensure("pull", "--quiet", testutil.AlpineImage)
		helpers.Ensure("image", "convert", "--estargz", "--oci", testutil.AlpineImage, esgz)
		helpers.Ensure("push", esgz)

		// An eStargz image whose layer doesn't match the diffID in the config
		client := newTestContainerdClient(helpers)
		defer client.Close()
		createInvalidESGZImage(context.Background(), helpers.T(), client, testutil.AlpineImage, invalid)
		helpers.Ensure("push", invalid)
		dumpImage(context.Background(), helpers.T(), client, esgz)
		dumpImage(context.Background(), helpers.T(), client, invalid)

		// Remove local copies so that the following pulls fetch from the registry
		// (tolerate refs that are already gone)
		helpers.Anyhow("rmi", "-f", esgz)
		helpers.Anyhow("rmi", "-f", invalid)
		// The base image may have been unpacked into the stargz snapshotter by the pull above. The snapshot
		// sharing the ChainID with the images in the registry would make the verifier refuse the first pull
		// below, so remove it (removal is synchronous, so its snapshots are gone afterwards).
		helpers.Ensure("rmi", "-f", testutil.AlpineImage)

		data.Labels().Set("esgz", esgz)
		data.Labels().Set("invalid", invalid)
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		if v := data.Labels().Get("esgz"); v != "" {
			helpers.Anyhow("rmi", "-f", v)
		}
		if v := data.Labels().Get("invalid"); v != "" {
			helpers.Anyhow("rmi", "-f", v)
		}
		if reg != nil {
			reg.Cleanup(data, helpers)
		}
	}

	testCase.Command = func(data test.Data, helpers test.Helpers) test.TestableCommand {
		client := newTestContainerdClient(helpers)
		defer client.Close()
		ctx := context.Background()
		dumpStargzSnapshots(ctx, helpers.T(), client, "before the first pull")
		helpers.Ensure("--snapshotter=stargz", "pull", data.Labels().Get("invalid"))
		dumpImage(ctx, helpers.T(), client, data.Labels().Get("invalid"))
		dumpStargzSnapshots(ctx, helpers.T(), client, "after pulling the invalid image")
		helpers.Ensure("--snapshotter=stargz", "images")
		return helpers.Command("--snapshotter=stargz", "pull", data.Labels().Get("esgz"))
	}

	testCase.Expected = test.Expects(expect.ExitCodeGenericFail, []error{errors.New("existing images contain invalid ChainIDs")}, nil)

	testCase.Run(t)
}
