# Docker-Based OCI Feature Pull

## Summary

Replace the `oras` CLI dependency for OCI feature pulling with Docker commands (`docker pull` + `docker save`). Docker is already a hard dependency of packnplay, so this eliminates an external tool requirement without adding new dependencies.

## Motivation

The current implementation requires users to install the `oras` CLI separately. When it's missing, users see:

```
failed to pull OCI feature ghcr.io/devcontainers/features/common-utils:2 (is 'oras' installed?)
```

Since Docker is already required to use packnplay, we can leverage it for OCI feature pulls instead.

## Design

### Approach

```
docker pull $ref
    ↓
docker save $ref → tar stream
    ↓
Parse manifest.json from tar → get layer path (e.g., "blobs/sha256/abc123")
    ↓
Extract layer blob from tar → pipe to system tar -xf
    ↓
Feature files now in cache directory
```

### Implementation

```go
func (r *FeatureResolver) pullOCIFeature(ociRef string) (string, error) {
    // 1. Setup cache directory (unchanged)
    featureCacheDir := filepath.Join(r.cacheDir, "oci-cache", nameVersion)

    // 2. Check cache hit (unchanged)
    if _, err := os.Stat(filepath.Join(featureCacheDir, "install.sh")); err == nil {
        return featureCacheDir, nil
    }

    // 3. Docker pull
    cmd := exec.Command("docker", "pull", "--quiet", ociRef)

    // 4. Docker save | parse manifest.json | extract layer
    saveCmd := exec.Command("docker", "save", ociRef)
    // Read tar stream, find manifest.json, get Layers[0] path
    // Extract that blob directly to featureCacheDir using system tar

    return featureCacheDir, nil
}
```

### Key Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Authentication | Trust Docker entirely | Users configure `docker login` the normal way |
| Security hardening | None (use system tar) | Matches pre-migration behavior; tar handles edge cases |
| Tar extraction | System `tar` command | Battle-tested, universally available |
| Image cleanup | Keep cached | Speeds up repeated pulls; users can `docker system prune` |

### Error Handling

| Scenario | Error Message |
|----------|---------------|
| Docker not available | `failed to pull OCI feature %s: %w` |
| Registry auth failure | `failed to pull OCI feature %s: %w` (Docker says "unauthorized") |
| Invalid OCI reference | `failed to pull OCI feature %s: %w` (Docker says "not found") |
| Corrupt/missing manifest | `failed to parse docker save output: manifest.json not found` |
| No layers in manifest | `failed to parse docker save output: no layers found` |
| Tar extraction failure | `failed to extract feature layer: %w` |

Philosophy: Let Docker's error messages do the heavy lifting.

### Testing

**In scope:**
- Pull public OCI feature (happy path)
- Cache hit behavior
- Invalid reference handling

**Out of scope:**
- Private registry auth (trust Docker)
- Malformed tar output (unlikely from Docker)
- Network failures (Docker handles retries)

Existing E2E tests that exercise OCI features should pass unchanged.

## Trade-offs

**Pros:**
- Eliminates `oras` CLI dependency
- ~50 lines of code
- No new Go dependencies
- Uses existing Docker authentication
- Docker images remain cached for faster subsequent pulls

**Cons:**
- Pulls full image to Docker cache (vs streaming with oras)
- Slightly more disk I/O (save + extract vs direct download)
- Relies on `docker save` output format stability

## Alternatives Considered

1. **Keep oras CLI** - Rejected: External dependency users must install
2. **oras-go library** - Rejected: 250 lines, 3 new dependencies, over-engineered
3. **docker create + docker cp** - Rejected: Doesn't work for OCI artifacts (no filesystem)
4. **HTTP-only implementation** - Rejected: Complex auth handling for private registries
