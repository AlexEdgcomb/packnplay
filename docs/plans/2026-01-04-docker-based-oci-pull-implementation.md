# Docker-Based OCI Feature Pull Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace `oras` CLI with Docker commands for pulling OCI features.

**Architecture:** Use `docker pull` to fetch the OCI artifact, then `docker save` piped through Go's tar reader to find the layer blob path from manifest.json, then extract that layer using system `tar`.

**Tech Stack:** Go stdlib (`archive/tar`, `os/exec`, `encoding/json`), Docker CLI

---

## Task 1: Replace pullOCIFeature with Docker-based implementation

**Files:**
- Modify: `pkg/devcontainer/features.go:186-245`

**Step 1: Read the current implementation**

Read `pkg/devcontainer/features.go` lines 180-250 to understand the current structure.

**Step 2: Replace the pullOCIFeature function**

Replace the entire `pullOCIFeature` function (lines 186-245) with:

```go
func (r *FeatureResolver) pullOCIFeature(ociRef string) (string, error) {
	// Create cache directory if it doesn't exist
	if err := os.MkdirAll(r.cacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Extract feature name for cache directory
	// e.g., ghcr.io/devcontainers/features/common-utils:2 -> common-utils-2
	parts := strings.Split(ociRef, "/")
	lastPart := parts[len(parts)-1]
	nameVersion := strings.ReplaceAll(lastPart, ":", "-")
	featureCacheDir := filepath.Join(r.cacheDir, "oci-cache", nameVersion)

	// Check if already cached
	if _, err := os.Stat(filepath.Join(featureCacheDir, "install.sh")); err == nil {
		return featureCacheDir, nil
	}

	// Create cache directory for extraction
	if err := os.MkdirAll(featureCacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create feature cache directory: %w", err)
	}

	// Pull the OCI artifact using Docker
	pullCmd := exec.Command("docker", "pull", "--quiet", ociRef)
	if output, err := pullCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to pull OCI feature %s: %w\nOutput: %s", ociRef, err, string(output))
	}

	// Use docker save to get the image layers, then extract the feature tarball
	saveCmd := exec.Command("docker", "save", ociRef)
	saveOutput, err := saveCmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create pipe for docker save: %w", err)
	}

	if err := saveCmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start docker save: %w", err)
	}

	// Parse the tar stream from docker save to find the layer blob
	layerPath, err := findLayerPathFromDockerSave(saveOutput)
	if err != nil {
		saveCmd.Wait()
		return "", fmt.Errorf("failed to parse docker save output: %w", err)
	}

	// We need to run docker save again to extract the actual layer
	// (the first run was just to find the layer path)
	saveCmd.Wait()

	// Now extract the layer blob using docker save piped to tar
	if err := extractLayerFromDockerSave(ociRef, layerPath, featureCacheDir); err != nil {
		return "", fmt.Errorf("failed to extract feature layer: %w", err)
	}

	return featureCacheDir, nil
}
```

**Step 3: Add the findLayerPathFromDockerSave helper function**

Add this function right after `pullOCIFeature`:

```go
// findLayerPathFromDockerSave parses the tar stream from docker save to find the layer blob path
func findLayerPathFromDockerSave(r io.Reader) (string, error) {
	tr := tar.NewReader(r)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		if header.Name == "manifest.json" {
			var manifests []struct {
				Layers []string `json:"Layers"`
			}
			if err := json.NewDecoder(tr).Decode(&manifests); err != nil {
				return "", fmt.Errorf("failed to decode manifest.json: %w", err)
			}
			if len(manifests) == 0 || len(manifests[0].Layers) == 0 {
				return "", fmt.Errorf("no layers found in manifest")
			}
			return manifests[0].Layers[0], nil
		}
	}

	return "", fmt.Errorf("manifest.json not found")
}
```

**Step 4: Add the extractLayerFromDockerSave helper function**

Add this function right after `findLayerPathFromDockerSave`:

```go
// extractLayerFromDockerSave runs docker save and extracts the specified layer to destDir
func extractLayerFromDockerSave(ociRef, layerPath, destDir string) error {
	saveCmd := exec.Command("docker", "save", ociRef)
	saveOutput, err := saveCmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := saveCmd.Start(); err != nil {
		return err
	}
	defer saveCmd.Wait()

	tr := tar.NewReader(saveOutput)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("layer %s not found in docker save output", layerPath)
		}
		if err != nil {
			return err
		}

		if header.Name == layerPath {
			// Found the layer blob - extract it using system tar
			tarCmd := exec.Command("tar", "-xf", "-", "-C", destDir)
			tarCmd.Stdin = tr
			if output, err := tarCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("tar extraction failed: %w\nOutput: %s", err, string(output))
			}
			return nil
		}
	}
}
```

**Step 5: Add tar import if needed**

Check if `archive/tar` is already imported. If not, add it to the imports at the top of the file:

```go
import (
	"archive/tar"
	// ... existing imports
)
```

**Step 6: Update the comment block above pullOCIFeature**

Replace lines 173-185 with:

```go
// pullOCIFeature pulls an OCI feature from a registry using Docker.
// Docker must be installed and available in PATH.
//
// Authentication is handled by Docker - users should configure registry
// credentials using `docker login` as they normally would.
```

**Step 7: Run the existing test**

Run: `go test ./pkg/devcontainer -run TestResolveOCIFeature -v`

Expected: PASS (the test should work with the new implementation)

**Step 8: Run full test suite**

Run: `go test ./pkg/devcontainer -v`

Expected: All tests pass

**Step 9: Run linter**

Run: `golangci-lint run ./pkg/devcontainer/...`

Expected: No errors

**Step 10: Commit**

```bash
git add pkg/devcontainer/features.go
git commit -m "refactor: replace oras CLI with Docker for OCI feature pulls

Use docker pull + docker save instead of oras CLI. This eliminates
the external oras dependency since Docker is already required.

- docker pull fetches the OCI artifact
- docker save exports it as a tar stream
- Parse manifest.json to find the layer blob path
- Extract layer blob using system tar

🤖 Generated with [Claude Code](https://claude.com/claude-code)

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Task 2: Test cache hit behavior

**Files:**
- None (manual verification)

**Step 1: Run test twice to verify caching**

Run: `go test ./pkg/devcontainer -run TestResolveOCIFeature -v -count=2`

Expected: Second run should be fast (cache hit)

**Step 2: Verify no duplicate docker pull on cache hit**

Add debug output or check that `install.sh` exists check works correctly by inspecting the cache directory after the first run.

---

## Task 3: Clean up design document commit

**Files:**
- Move: `docs/plans/2026-01-04-docker-based-oci-feature-pull-design.md`

**Step 1: Check if design doc was committed to wrong branch**

The design doc was committed while we were setting up. Verify it exists on this branch:

Run: `git log --oneline -3`

If the design doc commit exists, we're good. If not, cherry-pick or re-add it.

---

## Task 4: Run E2E tests

**Files:**
- None (verification only)

**Step 1: Run E2E tests that exercise OCI features**

Run: `go test ./... -tags=e2e -v -run OCI` (or the project's E2E test command)

Expected: All OCI-related E2E tests pass

**Step 2: If any failures, debug and fix**

Read the failure output and adjust the implementation as needed.
