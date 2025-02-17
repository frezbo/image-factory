// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build integration

package integration_test

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/siderolabs/gen/optional"
	"github.com/siderolabs/gen/xslices"
	"github.com/siderolabs/talos/pkg/machinery/extensions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/u-root/u-root/pkg/cpio"
	"github.com/ulikunitz/xz"
	"gopkg.in/yaml.v3"
)

type initramfsSpec struct {
	earlyPaths      []string
	extensions      []string
	modulesDepMatch optional.Optional[string]
	schematicID     string
}

func eatPadding(t *testing.T, in *bufio.Reader) {
	t.Helper()

	for {
		b, err := in.ReadByte()
		if err == io.EOF {
			return
		}

		require.NoError(t, err)

		if b != 0 {
			require.NoError(t, in.UnreadByte())

			return
		}
	}
}

func assertInitramfs(t *testing.T, path string, expected initramfsSpec) {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)

	in := bufio.NewReader(f)

	r := &discarder{r: in}

	if expected.earlyPaths != nil {
		// first section should be uncompressed
		var actualEarlyPaths []string

		cpioR := cpio.Newc.Reader(r)

		for {
			record, err := cpioR.ReadRecord()
			if err == io.EOF {
				break
			}

			require.NoError(t, err)

			if record.Name == "." {
				continue
			}

			actualEarlyPaths = append(actualEarlyPaths, record.Name)
		}

		for _, expectedPath := range expected.earlyPaths {
			assert.Contains(t, actualEarlyPaths, expectedPath)
		}

		eatPadding(t, in)
	}

	// this section should contain the original Talos initramfs, xz-compressed
	xzR, err := xz.NewReader(in)
	require.NoError(t, err)

	in = bufio.NewReader(xzR)

	r = &discarder{r: in}

	cpioR := cpio.Newc.Reader(r)

	for {
		record, err := cpioR.ReadRecord()
		if err == io.EOF {
			break
		}

		require.NoError(t, err)

		// skip over lib/firmware stuff
		if record.Name == "." || strings.HasPrefix(record.Name, "lib") {
			continue
		}

		assert.Contains(t, []string{"init", "rootfs.sqsh"}, record.Name)
	}

	eatPadding(t, in)

	r = &discarder{r: in}

	cpioR = cpio.Newc.Reader(r)

	var extensionConfig extensions.Config

	sqshPath := t.TempDir()

	for {
		record, err := cpioR.ReadRecord()
		if err == io.EOF {
			break
		}

		require.NoError(t, err)

		// decode extensions
		switch {
		case record.Name == "extensions.yaml":
			require.NoError(t, yaml.NewDecoder(record.ReaderAt.(*io.SectionReader)).Decode(&extensionConfig))
		case filepath.Ext(record.Name) == ".sqsh":
			sqshFile, err := os.Create(filepath.Join(sqshPath, record.Name))
			require.NoError(t, err)

			_, err = io.Copy(sqshFile, record.ReaderAt.(*io.SectionReader))
			require.NoError(t, err)

			require.NoError(t, sqshFile.Close())
		}
	}

	eatPadding(t, in)

	// should be EOF now
	_, err = in.Read(make([]byte, 1))
	require.Equal(t, io.EOF, err)

	require.NoError(t, f.Close())

	// assert on extensions
	expectedExtensions := 1 /* schematic */ + len(expected.extensions)

	if expected.modulesDepMatch.IsPresent() {
		expectedExtensions++
	}

	assert.Len(t, extensionConfig.Layers, expectedExtensions)

	actualNames := xslices.Map(extensionConfig.Layers, func(e *extensions.Layer) string {
		return e.Metadata.Name
	})
	expectedNames := append(slices.Clone(expected.extensions), "schematic")

	if expected.modulesDepMatch.IsPresent() {
		expectedNames = append(expectedNames, "modules.dep")
	}

	assert.Equal(t, expectedNames, actualNames)

	// assert on schematic
	schematicIdx := slices.Index(expectedNames, "schematic")
	assert.Equal(t, expected.schematicID, extensionConfig.Layers[schematicIdx].Metadata.Version)

	// assert on modules.dep being rebuilt
	if expected.modulesDepMatch.IsPresent() {
		// find & extract squashfs
		modulesDepIdx := slices.Index(expectedNames, "modules.dep")
		modulesSqshPath := filepath.Join(sqshPath, extensionConfig.Layers[modulesDepIdx].Image)
		dest := t.TempDir()

		require.NoError(t, exec.Command("unsquashfs", "-d", dest, modulesSqshPath).Run())

		modulesDepPath, err := filepath.Glob(filepath.Join(dest, "lib/modules/*/modules.dep"))
		require.NoError(t, err)
		require.Len(t, modulesDepPath, 1)

		contents, err := os.ReadFile(modulesDepPath[0])
		require.NoError(t, err)

		assert.Contains(t, string(contents), expected.modulesDepMatch.ValueOrZero())
		assert.Contains(t, string(contents), "mlxfw.ko") // assert on a known module from base initramfs
	}
}
