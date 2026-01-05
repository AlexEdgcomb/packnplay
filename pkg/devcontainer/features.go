package devcontainer

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// OptionSpec represents a feature option specification
type OptionSpec struct {
	Type        string      `json:"type"`
	Default     interface{} `json:"default,omitempty"`
	Description string      `json:"description,omitempty"`
	Proposals   []string    `json:"proposals,omitempty"`
}

// Mount represents a mount specification from feature metadata
type Mount struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
}

// FeatureMetadata represents the metadata from devcontainer-feature.json
// Enhanced to support complete devcontainer-feature.json specification
type FeatureMetadata struct {
	// Required fields per specification
	ID      string `json:"id"`
	Version string `json:"version"`
	Name    string `json:"name"`

	// Optional description
	Description string `json:"description,omitempty"`

	// Options specification
	Options map[string]OptionSpec `json:"options,omitempty"`

	// Container properties that features can contribute
	ContainerEnv map[string]string `json:"containerEnv,omitempty"`
	Privileged   *bool             `json:"privileged,omitempty"`
	Init         *bool             `json:"init,omitempty"`
	CapAdd       []string          `json:"capAdd,omitempty"`
	SecurityOpt  []string          `json:"securityOpt,omitempty"`
	Entrypoint   []string          `json:"entrypoint,omitempty"`
	Mounts       []Mount           `json:"mounts,omitempty"`

	// Lifecycle hooks that features can contribute
	OnCreateCommand      *LifecycleCommand `json:"onCreateCommand,omitempty"`
	UpdateContentCommand *LifecycleCommand `json:"updateContentCommand,omitempty"`
	PostCreateCommand    *LifecycleCommand `json:"postCreateCommand,omitempty"`
	PostStartCommand     *LifecycleCommand `json:"postStartCommand,omitempty"`
	PostAttachCommand    *LifecycleCommand `json:"postAttachCommand,omitempty"`

	// Dependencies - Microsoft spec compliance
	DependsOn     map[string]interface{} `json:"dependsOn,omitempty"`     // Feature IDs mapping to options
	InstallsAfter []string               `json:"installsAfter,omitempty"` // Simple feature ID list
}

// UnmarshalJSON implements custom JSON unmarshaling to handle both string and array formats for entrypoint
func (f *FeatureMetadata) UnmarshalJSON(data []byte) error {
	// Create a temporary struct with Entrypoint removed to avoid infinite recursion
	type Alias struct {
		ID                   string                 `json:"id"`
		Version              string                 `json:"version"`
		Name                 string                 `json:"name"`
		Description          string                 `json:"description,omitempty"`
		Options              map[string]OptionSpec  `json:"options,omitempty"`
		ContainerEnv         map[string]string      `json:"containerEnv,omitempty"`
		Privileged           *bool                  `json:"privileged,omitempty"`
		Init                 *bool                  `json:"init,omitempty"`
		CapAdd               []string               `json:"capAdd,omitempty"`
		SecurityOpt          []string               `json:"securityOpt,omitempty"`
		Mounts               []Mount                `json:"mounts,omitempty"`
		OnCreateCommand      *LifecycleCommand      `json:"onCreateCommand,omitempty"`
		UpdateContentCommand *LifecycleCommand      `json:"updateContentCommand,omitempty"`
		PostCreateCommand    *LifecycleCommand      `json:"postCreateCommand,omitempty"`
		PostStartCommand     *LifecycleCommand      `json:"postStartCommand,omitempty"`
		PostAttachCommand    *LifecycleCommand      `json:"postAttachCommand,omitempty"`
		DependsOn            map[string]interface{} `json:"dependsOn,omitempty"`
		InstallsAfter        []string               `json:"installsAfter,omitempty"`
	}

	var aux Alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Copy all fields except entrypoint
	f.ID = aux.ID
	f.Version = aux.Version
	f.Name = aux.Name
	f.Description = aux.Description
	f.Options = aux.Options
	f.ContainerEnv = aux.ContainerEnv
	f.Privileged = aux.Privileged
	f.Init = aux.Init
	f.CapAdd = aux.CapAdd
	f.SecurityOpt = aux.SecurityOpt
	f.Mounts = aux.Mounts
	f.OnCreateCommand = aux.OnCreateCommand
	f.UpdateContentCommand = aux.UpdateContentCommand
	f.PostCreateCommand = aux.PostCreateCommand
	f.PostStartCommand = aux.PostStartCommand
	f.PostAttachCommand = aux.PostAttachCommand
	f.DependsOn = aux.DependsOn
	f.InstallsAfter = aux.InstallsAfter

	// Handle entrypoint field specially - it can be string or array
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if entrypointRaw, exists := raw["entrypoint"]; exists {
		// Try to unmarshal as string first
		var entrypointStr string
		if err := json.Unmarshal(entrypointRaw, &entrypointStr); err == nil {
			// It's a string - convert to array
			f.Entrypoint = []string{entrypointStr}
		} else {
			// Try as array
			var entrypointArr []string
			if err := json.Unmarshal(entrypointRaw, &entrypointArr); err != nil {
				return fmt.Errorf("entrypoint must be either a string or an array of strings: %w", err)
			}
			f.Entrypoint = entrypointArr
		}
	}

	return nil
}

// ResolvedFeature represents a feature that has been resolved and is ready for installation
type ResolvedFeature struct {
	ID            string
	Version       string
	InstallPath   string
	Options       map[string]interface{}
	Metadata      *FeatureMetadata
	DependsOn     map[string]interface{} // Feature IDs to options mapping
	InstallsAfter []string
}

// FeatureResolver handles resolving features from various sources
type FeatureResolver struct {
	cacheDir string
	lockfile *LockFile // Optional lockfile for version pinning
}

// NewFeatureResolver creates a new FeatureResolver with the specified cache directory and optional lockfile
func NewFeatureResolver(cacheDir string, lockfile *LockFile) *FeatureResolver {
	return &FeatureResolver{
		cacheDir: cacheDir,
		lockfile: lockfile,
	}
}

// isOCIReference checks if a feature reference is an OCI registry reference
func isOCIReference(ref string) bool {
	// OCI references contain : (for version) or start with registry domains
	return strings.Contains(ref, "ghcr.io/") || strings.Contains(ref, "mcr.microsoft.com/")
}

// pullOCIFeature pulls an OCI feature to the cache directory
//
// Authentication: This function automatically inherits Docker credentials from ~/.docker/config.json.
// Users can authenticate to private registries using standard Docker login:
//
//	docker login ghcr.io
//	docker login myregistry.com
//
// The oras-go library automatically reads credentials from the same location as Docker,
// enabling seamless access to private features without additional configuration.
// See: https://oras.land/docs/how_to_guides/authentication/
//
// For credential helpers (Docker Desktop, cloud provider helpers), those are also inherited
// automatically, as they're configured in the same Docker config file.
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

	// Create context with timeout for the pull operation
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Connect to the remote repository
	repo, err := remote.NewRepository(ociRef)
	if err != nil {
		return "", fmt.Errorf("failed to create repository for %s: %w", ociRef, err)
	}

	// Set up authentication using Docker credentials
	credStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		// If we can't load Docker credentials, continue without auth (for public registries)
		// This matches the behavior of the oras CLI
		repo.Client = retry.DefaultClient
	} else {
		repo.Client = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: credentials.Credential(credStore),
		}
	}

	// Parse the tag from the reference (e.g., "2" from "ghcr.io/devcontainers/features/common-utils:2")
	// Handle digest references (@sha256:...) by stripping the digest portion first
	tag := "latest"
	refForTag := ociRef
	if atIdx := strings.LastIndex(ociRef, "@"); atIdx != -1 {
		// Reference contains a digest (e.g., "ghcr.io/repo:tag@sha256:abc123")
		// Strip the digest to find the tag in the remaining portion
		refForTag = ociRef[:atIdx]
	}
	if idx := strings.LastIndex(refForTag, ":"); idx != -1 {
		possibleTag := refForTag[idx+1:]
		// Make sure it's not a port number (check if we're past any slash)
		if !strings.Contains(possibleTag, "/") {
			tag = possibleTag
		}
	}

	// Resolve the manifest to get the layer information
	manifestDesc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return "", fmt.Errorf("failed to resolve manifest for %s: %w", ociRef, err)
	}

	// Fetch the manifest content
	manifestReader, err := repo.Fetch(ctx, manifestDesc)
	if err != nil {
		return "", fmt.Errorf("failed to fetch manifest for %s: %w", ociRef, err)
	}
	manifestBytes, err := io.ReadAll(manifestReader)
	manifestReader.Close()
	if err != nil {
		return "", fmt.Errorf("failed to read manifest: %w", err)
	}

	// Parse the manifest to find layers
	var manifest struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return "", fmt.Errorf("failed to parse manifest: %w", err)
	}

	if len(manifest.Layers) == 0 {
		return "", fmt.Errorf("no layers found in OCI feature manifest")
	}

	// Fetch the first layer (the feature tarball)
	// Devcontainer features have a single layer which is the .tgz file
	layer := manifest.Layers[0]

	// Create a descriptor for the layer
	layerDesc := ocispec.Descriptor{
		MediaType: layer.MediaType,
		Digest:    digest.Digest(layer.Digest),
		Size:      layer.Size,
	}

	// Fetch the layer content
	layerReader, err := repo.Fetch(ctx, layerDesc)
	if err != nil {
		return "", fmt.Errorf("failed to fetch layer: %w", err)
	}
	defer layerReader.Close()

	// Save the layer to a temporary file
	tarballPath := filepath.Join(featureCacheDir, "feature.tgz")
	tarballFile, err := os.Create(tarballPath)
	if err != nil {
		return "", fmt.Errorf("failed to create tarball file: %w", err)
	}

	// Copy with size limit to prevent issues (read one extra byte to detect oversized layers)
	const maxLayerSize = 100 * 1024 * 1024 // 100MB
	n, err := io.CopyN(tarballFile, layerReader, maxLayerSize+1)
	tarballFile.Close()
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("failed to write layer content: %w", err)
	}
	if n > maxLayerSize {
		return "", fmt.Errorf("feature layer exceeds maximum size of 100MB")
	}

	// Extract tarball to the cache directory
	// The extractTarball function auto-detects compression via magic bytes
	if err := extractTarball(tarballPath, featureCacheDir, layer.MediaType); err != nil {
		return "", fmt.Errorf("failed to extract tarball: %w", err)
	}

	// Remove the tarball after extraction
	_ = os.Remove(tarballPath)

	return featureCacheDir, nil
}

// extractTarball extracts a tarball to the specified directory
// It handles both gzip-compressed and uncompressed tar archives based on file magic bytes
func extractTarball(tarballPath, destDir, mediaType string) error {
	f, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("failed to open tarball: %w", err)
	}
	defer f.Close()

	// Check for gzip magic bytes (1f 8b) to detect compression
	// This is the most reliable way - file magic bytes are the source of truth
	magic := make([]byte, 2)
	if _, err := f.Read(magic); err != nil {
		return fmt.Errorf("failed to read file header: %w", err)
	}
	// Seek back to start
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek: %w", err)
	}

	var tr *tar.Reader
	// Only use gzip if magic bytes confirm it's gzip (0x1f 0x8b)
	// Don't rely on file extension or media type as they can be misleading
	isGzip := magic[0] == 0x1f && magic[1] == 0x8b

	if isGzip {
		gzr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gzr.Close()
		tr = tar.NewReader(gzr)
	} else {
		tr = tar.NewReader(f)
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Sanitize the path to prevent directory traversal attacks
		// Clean the path and ensure it doesn't escape the destination directory
		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			continue // Skip potentially dangerous paths
		}

		target := filepath.Join(destDir, cleanName)

		// Ensure the target is within destDir (defense in depth)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)) {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", target, err)
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("failed to create parent directory for %s: %w", target, err)
			}

			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("failed to create file %s: %w", target, err)
			}

			// Limit copy size to prevent decompression bombs (100MB per file)
			const maxFileSize = 100 * 1024 * 1024
			n, err := io.CopyN(outFile, tr, maxFileSize+1)
			outFile.Close()
			if err != nil && err != io.EOF {
				return fmt.Errorf("failed to write file %s: %w", target, err)
			}
			if n > maxFileSize {
				return fmt.Errorf("file %s exceeds maximum size of 100MB", target)
			}
		case tar.TypeSymlink:
			// Handle symlinks - ensure they don't escape destDir
			linkTarget := header.Linkname
			if filepath.IsAbs(linkTarget) {
				continue // Skip absolute symlinks
			}
			resolvedLink := filepath.Join(filepath.Dir(target), linkTarget)
			if !strings.HasPrefix(filepath.Clean(resolvedLink), filepath.Clean(destDir)) {
				continue // Skip symlinks that would escape destDir
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				// Ignore symlink errors - some systems may not support them
				continue
			}
		}
	}

	return nil
}

// hashURL generates a cache-safe hash of a URL
func hashURL(url string) string {
	hash := sha256.Sum256([]byte(url))
	return hex.EncodeToString(hash[:])
}

// downloadHTTPSFeature downloads a feature from an HTTPS/HTTP URL and extracts it to the cache
func (r *FeatureResolver) downloadHTTPSFeature(url string) (string, error) {
	// Create cache directory if it doesn't exist
	if err := os.MkdirAll(r.cacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Create cache directory for this specific URL
	urlHash := hashURL(url)
	featureCacheDir := filepath.Join(r.cacheDir, "https-cache", urlHash)

	// Check if already cached
	if _, err := os.Stat(filepath.Join(featureCacheDir, "install.sh")); err == nil {
		return featureCacheDir, nil
	}

	// Create cache directory
	if err := os.MkdirAll(featureCacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create feature cache directory: %w", err)
	}

	// Download tarball with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to download feature from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to download feature: HTTP %d", resp.StatusCode)
	}

	// Validate Content-Type to ensure it's a tarball
	contentType := resp.Header.Get("Content-Type")
	validContentTypes := []string{
		"application/x-gzip",
		"application/gzip",
		"application/x-tar",
		"application/x-compressed-tar",
		"application/octet-stream", // Many servers use this generic type
	}
	isValidType := false
	for _, validType := range validContentTypes {
		if strings.Contains(contentType, validType) {
			isValidType = true
			break
		}
	}
	if !isValidType && contentType != "" {
		return "", fmt.Errorf("invalid content type for feature tarball: %s (expected gzip/tar archive)", contentType)
	}

	// Create temporary file for tarball
	tmpFile, err := os.CreateTemp("", "feature-*.tgz")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write response to temp file with size limit
	const maxFeatureSize = 100 * 1024 * 1024 // 100MB
	limitedReader := io.LimitReader(resp.Body, maxFeatureSize)
	n, err := io.Copy(tmpFile, limitedReader)
	if err != nil {
		return "", fmt.Errorf("failed to write tarball: %w", err)
	}
	if n == maxFeatureSize {
		return "", fmt.Errorf("feature tarball exceeds maximum size of 100MB")
	}

	// Close file before extraction
	tmpFile.Close()

	// Extract tarball to cache directory using Go's archive/tar
	// The extractTarball function handles security (path traversal prevention)
	if err := extractTarball(tmpFile.Name(), featureCacheDir, contentType); err != nil {
		return "", fmt.Errorf("failed to extract tarball: %w", err)
	}

	return featureCacheDir, nil
}

// ResolveFeature resolves a local feature from the given path with the specified options
func (r *FeatureResolver) ResolveFeature(featurePath string, options map[string]interface{}) (*ResolvedFeature, error) {
	// Check if lockfile has a pinned version for this feature
	if r.lockfile != nil {
		if locked, exists := r.lockfile.Features[featurePath]; exists {
			// Use the locked/resolved version instead of the original reference
			featurePath = locked.Resolved
		}
	}

	// Check if this is an OCI reference
	if isOCIReference(featurePath) {
		cachedPath, err := r.pullOCIFeature(featurePath)
		if err != nil {
			return nil, err
		}
		featurePath = cachedPath
	}

	// Check if this is an HTTPS tarball
	if strings.HasPrefix(featurePath, "https://") || strings.HasPrefix(featurePath, "http://") {
		cachedPath, err := r.downloadHTTPSFeature(featurePath)
		if err != nil {
			return nil, err
		}
		featurePath = cachedPath
	}
	// Read metadata from devcontainer-feature.json if it exists
	metadataPath := filepath.Join(featurePath, "devcontainer-feature.json")
	metadataBytes, err := os.ReadFile(metadataPath)

	var metadata FeatureMetadata
	if err == nil {
		// Metadata file exists - parse it
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			return nil, fmt.Errorf("failed to parse feature metadata: %w", err)
		}
	} else if os.IsNotExist(err) {
		// Metadata file doesn't exist - use minimal defaults for local features
		// Use the directory name as the feature ID
		metadata = FeatureMetadata{
			ID:      filepath.Base(featurePath),
			Version: "1.0.0",
		}
	} else {
		// Some other error reading the file
		return nil, fmt.Errorf("failed to read feature metadata: %w", err)
	}

	// Create resolved feature
	resolved := &ResolvedFeature{
		ID:            metadata.ID,
		Version:       metadata.Version,
		InstallPath:   featurePath,
		Options:       options,
		Metadata:      &metadata,
		DependsOn:     metadata.DependsOn,
		InstallsAfter: metadata.InstallsAfter,
	}

	return resolved, nil
}

// ResolveFeaturesWithOverride resolves features with optional manual ordering override
// If overrideOrder is nil or empty, uses dependency-based resolution
// If overrideOrder is provided, uses that order for specified features, then appends remaining features
// Prints warnings to stderr if override order doesn't include all features
func (r *FeatureResolver) ResolveFeaturesWithOverride(features map[string]*ResolvedFeature, overrideOrder []string) ([]*ResolvedFeature, error) {
	// If no override specified, use normal dependency resolution
	if len(overrideOrder) == 0 {
		return r.ResolveFeatures(features)
	}

	// Load metadata for all features (needed for validation)
	for id, feature := range features {
		if feature.Metadata == nil {
			metadataPath := filepath.Join(feature.InstallPath, "devcontainer-feature.json")
			metadataBytes, err := os.ReadFile(metadataPath)

			var metadata FeatureMetadata
			if err == nil {
				if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
					return nil, fmt.Errorf("failed to parse metadata for feature %s: %w", id, err)
				}
			} else if os.IsNotExist(err) {
				metadata = FeatureMetadata{
					ID:      id,
					Version: "1.0.0",
				}
			} else {
				return nil, fmt.Errorf("failed to read metadata for feature %s: %w", id, err)
			}
			feature.Metadata = &metadata
		}
	}

	// Build result using override order
	var result []*ResolvedFeature
	used := make(map[string]bool)

	// First, add features in override order
	for _, featureID := range overrideOrder {
		if feature, exists := features[featureID]; exists {
			result = append(result, feature)
			used[featureID] = true
		}
	}

	// Check if any features are missing from override order
	var missing []string
	for id := range features {
		if !used[id] {
			missing = append(missing, id)
		}
	}

	// Warn if override order doesn't include all features
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "Warning: overrideFeatureInstallOrder does not include all features. Missing: %v\n", missing)
		fmt.Fprintf(os.Stderr, "         These features will be installed after the specified order.\n")
	}

	// Then add any remaining features (in map iteration order)
	for id, feature := range features {
		if !used[id] {
			result = append(result, feature)
		}
	}

	return result, nil
}

// ResolveFeatures resolves feature dependencies and returns features in installation order
func (r *FeatureResolver) ResolveFeatures(features map[string]*ResolvedFeature) ([]*ResolvedFeature, error) {
	// Load metadata for all features
	featureMetadata := make(map[string]*FeatureMetadata)
	for id, feature := range features {
		metadataPath := filepath.Join(feature.InstallPath, "devcontainer-feature.json")
		metadataBytes, err := os.ReadFile(metadataPath)

		var metadata FeatureMetadata
		if err == nil {
			// Metadata file exists - parse it
			if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
				return nil, fmt.Errorf("failed to parse metadata for feature %s: %w", id, err)
			}
		} else if os.IsNotExist(err) {
			// Metadata file doesn't exist - use minimal defaults
			metadata = FeatureMetadata{
				ID:      id,
				Version: "1.0.0",
			}
		} else {
			// Some other error reading the file
			return nil, fmt.Errorf("failed to read metadata for feature %s: %w", id, err)
		}

		featureMetadata[id] = &metadata
		// Update the feature with dependency info
		feature.DependsOn = metadata.DependsOn
		feature.InstallsAfter = metadata.InstallsAfter
	}

	// Round-based resolution algorithm
	var result []*ResolvedFeature
	installed := make(map[string]bool)
	remaining := make(map[string]*ResolvedFeature)
	for id, feature := range features {
		remaining[id] = feature
	}

	for len(remaining) > 0 {
		var roundInstalls []*ResolvedFeature

		// Try to find features that can be installed in this round
		for _, feature := range remaining {
			// Check if all hard dependencies (dependsOn) are satisfied
			canInstall := true
			for depID := range feature.DependsOn {
				if !installed[depID] {
					canInstall = false
					break
				}
			}

			// Check if all soft dependencies (installsAfter) are satisfied or not in the set
			if canInstall {
				for _, afterID := range feature.InstallsAfter {
					// Only block if the feature exists in our set and isn't installed yet
					if _, exists := features[afterID]; exists && !installed[afterID] {
						canInstall = false
						break
					}
				}
			}

			if canInstall {
				roundInstalls = append(roundInstalls, feature)
			}
		}

		// If no features can be installed, we have an error
		if len(roundInstalls) == 0 {
			// Build list of remaining features for error message
			var remainingIDs []string
			for id := range remaining {
				remainingIDs = append(remainingIDs, id)
			}
			return nil, fmt.Errorf("cannot resolve dependencies: features %v have unsatisfied dependencies", remainingIDs)
		}

		// Install this round's features
		for _, feature := range roundInstalls {
			result = append(result, feature)
			installed[feature.ID] = true
			delete(remaining, feature.ID)
		}
	}

	return result, nil
}

// FeatureOptionsProcessor handles option to environment variable conversion
type FeatureOptionsProcessor struct{}

// NewFeatureOptionsProcessor creates a new options processor
func NewFeatureOptionsProcessor() *FeatureOptionsProcessor {
	return &FeatureOptionsProcessor{}
}

// ValidateAndProcessOptions validates feature options and converts to environment variables
func (p *FeatureOptionsProcessor) ValidateAndProcessOptions(userOptions map[string]interface{}, optionSpecs map[string]OptionSpec) (map[string]string, error) {
	// First validate all user-provided options
	for optionName, userValue := range userOptions {
		spec, exists := optionSpecs[optionName]
		if !exists {
			// Option not in spec - skip validation
			continue
		}

		if err := p.validateOptionValue(optionName, userValue, spec); err != nil {
			return nil, err
		}
	}

	// Then process options (apply defaults, convert to env vars)
	return p.ProcessOptions(userOptions, optionSpecs), nil
}

// validateOptionValue validates a single option value against its spec
func (p *FeatureOptionsProcessor) validateOptionValue(optionName string, value interface{}, spec OptionSpec) error {
	// Validate type
	switch spec.Type {
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("option '%s' must be of type string", optionName)
		}
		// Validate enum (proposals)
		if len(spec.Proposals) > 0 {
			strValue := value.(string)
			valid := false
			for _, proposal := range spec.Proposals {
				if strValue == proposal {
					valid = true
					break
				}
			}
			if !valid {
				return fmt.Errorf("option '%s' value '%s' must be one of: %v", optionName, strValue, spec.Proposals)
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("option '%s' must be of type boolean", optionName)
		}
	case "number":
		// Accept int, int64, float64
		switch value.(type) {
		case int, int64, float64:
			// Valid number types
		default:
			return fmt.Errorf("option '%s' must be of type number", optionName)
		}
	}

	return nil
}

// ProcessOptions converts feature options to environment variables per specification
func (p *FeatureOptionsProcessor) ProcessOptions(userOptions map[string]interface{}, optionSpecs map[string]OptionSpec) map[string]string {
	result := make(map[string]string)

	// Process all option specs (apply defaults, then user overrides)
	for optionName, spec := range optionSpecs {
		envName := normalizeOptionName(optionName)

		// Start with default value
		value := spec.Default

		// Override with user value if provided
		if userValue, exists := userOptions[optionName]; exists {
			value = userValue
		}

		// Convert to string
		if value != nil {
			result[envName] = fmt.Sprintf("%v", value)
		}
	}

	return result
}

// normalizeOptionName converts option name to environment variable per specification
func normalizeOptionName(name string) string {
	// Per Microsoft spec: replace non-word chars with underscore, then replace leading digits and underscores with single underscore, uppercase
	// This matches Microsoft's getSafeId exactly:
	// str.replace(/[^\w_]/g, '_').replace(/^[\d_]+/g, '_').toUpperCase()

	re := regexp.MustCompile(`[^\w_]`)
	normalized := re.ReplaceAllString(name, "_")

	re2 := regexp.MustCompile(`^[\d_]+`)
	normalized = re2.ReplaceAllString(normalized, "_")

	return strings.ToUpper(normalized)
}
