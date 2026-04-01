# Debugging and Troubleshooting

## Log Levels

rclone-fusiondata uses rclone's standard logging system with four levels:

| Flag | Level | What You See |
|------|-------|-------------|
| *(default)* | NOTICE | Important notices and all errors |
| `-v` | INFO | Operations, transfers, retries, change detection |
| `-vv` | DEBUG | Full API trace, cache activity, path resolution steps |
| `-q` | ERROR | Errors only |

### Examples

```bash
# Default — errors and notices
rclone ls Fusion-ADSK:

# Verbose — see what operations are happening
rclone ls Fusion-ADSK: -v

# Full debug — trace every API call
rclone ls Fusion-ADSK: -vv

# Log to file (useful for mounted drives)
rclone mount Fusion-ADSK: ~/FusionData --vfs-cache-mode full -vv \
  --log-file=/tmp/fusiondata.log

# Errors only
rclone ls Fusion-ADSK: -q
```

## What Each Level Shows

### INFO level (`-v`)

| Component | Log Message | Meaning |
|-----------|------------|---------|
| fusiondata.go | `NewFs: hub=%q region=%q cacheTTL=%v` | Backend initialized |
| fusiondata.go | `List: dir=%q returned %d entries` | Directory listed |
| fusiondata.go | `ChangeNotify: poll cycle, checking for changes` | Polling for server-side changes |
| fusiondata.go | `ChangeNotify: detected %d changes` | Server-side changes found |
| graphql.go | `gqlQuery: rate limited (429), will retry` | API throttling hit |
| rest.go | `doAPIRequest: rate limited (429)...` | REST API throttling hit |
| rest.go | `uploadMultipart: uploading part %d/%d` | Large file upload progress |
| object.go | `Open: download started for %q size=%d` | File download began |
| object.go | `Update: new version created for %q` | File version created |

### DEBUG level (`-vv`)

All INFO messages plus:

| Component | Log Message | Meaning |
|-----------|------------|---------|
| **Path Resolution** | | |
| path.go | `resolvePath: path=%q` | Starting to resolve a path |
| path.go | `resolvePath: segment[0]=%q resolved to project id=%q` | Project found |
| path.go | `resolvePath: segment[N]=%q resolved to %s id=%q` | Each path segment resolved |
| path.go | `findChildByName: cache hit for %q in parent=%q` | Cache was used |
| path.go | `findChildByName: cache miss for %q in parent=%q, fetching` | API call needed |
| path.go | `listChildren: parentKind=%q parentID=%q` | Listing children of a node |
| path.go | `listChildren: hub returned %d projects` | Projects found |
| path.go | `listChildren: project returned %d children (...)` | Project contents |
| path.go | `listChildren: folder returned %d children` | Folder contents |
| **GraphQL** | | |
| graphql.go | `gqlQuery: executing %q vars=%v` | Query being sent |
| graphql.go | `gqlQuery: retry attempt %d/%d delay=%v` | Retrying failed query |
| graphql.go | `throttle: acquiring slot` | Waiting for rate limiter |
| graphql.go | `throttle: slot acquired` | Rate limiter passed |
| graphql.go | `throttle: slot released` | Request complete |
| **REST API** | | |
| rest.go | `doAPIRequest: %s %s` | HTTP request being made |
| rest.go | `doAPIRequest: retry attempt %d/%d delay=%v` | Retrying failed request |
| rest.go | `createStorage: project=%q folder=%q filename=%q` | Creating upload storage |
| rest.go | `uploadToStorage: ... using single part` | Upload method chosen |
| rest.go | `uploadToStorage: ... using multipart (chunkSize=%d)` | Multipart upload |
| rest.go | `createFirstVersion: ...` | New item being created |
| rest.go | `createNextVersion: ...` | New version being created |
| rest.go | `getTopFolders: ...returned %d folders` | DM API folder listing |
| rest.go | `getFolderContents: ...returned %d entries` | DM API contents listing |
| rest.go | `resolveFolderDMPath: found %q at top level` | Folder DM ID resolved |
| rest.go | `createFolder: ...` | Folder being created |
| **Queries** | | |
| queries.go | `GetHubs: querying hubs` | Listing hubs |
| queries.go | `GetHubs: found %d hubs` | Hubs returned |
| queries.go | `GetProjects: hubID=%q` | Listing projects |
| queries.go | `GetProjects: found %d active projects` | Projects returned |
| queries.go | `GetFolders: projectID=%q` | Listing folders |
| queries.go | `GetItems: hubID=%q folderID=%q` | Listing folder items |
| queries.go | `GetItemDetails: hubID=%q itemID=%q` | Fetching item metadata |
| **Cache** | | |
| cache.go | `cache.replaceChildren: parentID=%q count=%d` | Cache atomically updated |
| cache.go | `cache.invalidate: parentID=%q` | Cache cleared for directory |
| **Object** | | |
| object.go | `Open: downloading %q (id=%q)` | Starting download |
| object.go | `Open: resolved DM IDs project=%q folder=%q item=%q` | DM IDs found |
| object.go | `Update: updating %q size=%d` | Starting update |

## Common Issues and Solutions

### Token Expired

```
ERROR: oauth2: "invalid_grant" "The refresh token is invalid or expired."
```

**Cause:** Public PKCE refresh tokens expire after 14 days of non-use.

**Fix:**
```bash
rclone config reconnect Fusion-ADSK:
```

### Rate Limited

```
ERROR: rate limited (HTTP 429)
ERROR: GraphQL errors: Too many requests on downstream service
```

**Cause:** Too many API calls in a short period.

**Fix:** The backend auto-retries with exponential backoff. If persistent, lower the rate limit:
```bash
rclone ls Fusion-ADSK: --fusiondata-rate-limit 3
```

### Folder Not Found via DM API

```
ERROR: top folder "FolderName" not found via DM API
ERROR: folder "FolderName" not found via DM API
```

**Cause:** The DM REST API has an invisible root folder layer between projects and user-visible folders. The backend searches inside top folders automatically, but newly created folders may not be found immediately due to caching.

**Fix:** Wait for cache TTL to expire, or reduce it:
```bash
rclone ls Fusion-ADSK: --fusiondata-cache-ttl 30s
```

### Cannot Determine Data Management IDs

```
ERROR: cannot determine Data Management IDs for download
ERROR: cannot determine project for upload
```

**Cause:** Path resolution failed to find the GraphQL or DM API IDs for the item.

**Fix:** Run with `-vv` to trace the path resolution and identify which segment fails:
```bash
rclone ls Fusion-ADSK:ProjectName/FolderName/ -vv 2>&1 | grep resolvePath
```

### macOS Safe-Save Errors

```
ERROR: Dir.Stat error: object not found
ERROR: .sb-409cc62d-XXXXX
```

**Cause:** macOS apps use atomic save (temp file + rename). The backend silently handles these patterns.

**Fix:** These are usually harmless. If persistent, ensure you're using the latest build which includes temp file detection for `.sb-*`, `.~`, and `~$` patterns.

### Mount Shows Empty / Stale Content

**Cause:** Directory cache hasn't expired yet.

**Fix:** Use shorter cache times and enable polling:
```bash
rclone mount Fusion-ADSK: ~/FusionData \
  --vfs-cache-mode full \
  --dir-cache-time 1m \
  --poll-interval 1m \
  --fusiondata-cache-ttl 1m
```

Or manually refresh via remote control:
```bash
# Start mount with RC enabled
rclone mount Fusion-ADSK: ~/FusionData --vfs-cache-mode full --rc

# From another terminal — refresh a specific directory
rclone rc vfs/refresh dir=ProjectName/FolderName

# Refresh everything
rclone rc vfs/refresh
```

### Upload Failed: Legacy Endpoint Deprecated

```
ERROR: upload failed (HTTP 403): {"reason":"Legacy endpoint is deprecated"}
```

**Cause:** Using an old build that calls the deprecated OSS v2 direct PUT endpoint.

**Fix:** Rebuild with the latest source. The current version uses signed S3 URLs for all uploads and downloads.

### Permission Denied

```
ERROR: access denied (HTTP 403)
ERROR: unauthorized — token may be expired
```

**Cause:** Token lacks required scopes, or the user doesn't have access to the hub/project.

**Fix:**
1. Re-authenticate: `rclone config reconnect Fusion-ADSK:`
2. Ensure your APS app has scopes: `data:read data:write data:create user-profile:read`
3. Verify hub access in the Fusion Team web interface

## Performance Tuning

### Reduce API Calls

```bash
# Increase cache TTL (less frequent API calls, slower change detection)
--fusiondata-cache-ttl 10m --dir-cache-time 10m
```

### Improve Upload Speed

```bash
# Adjust chunk size for large files (default 100MB)
--fusiondata-upload-chunk-size 200M
```

### Optimize Mount Performance

```bash
rclone mount Fusion-ADSK: ~/FusionData \
  --vfs-cache-mode full \
  --vfs-cache-max-age 2h \
  --vfs-read-ahead 128M \
  --dir-cache-time 2m \
  --attr-timeout 1s \
  --fusiondata-cache-ttl 2m \
  --fusiondata-rate-limit 5
```

### Debug a Specific Operation

```bash
# Trace a download
rclone copy Fusion-ADSK:Project/file.f3d ./local/ -vv 2>&1 | grep -E "(Open|download|resolve)"

# Trace an upload
rclone copy ./file.pdf Fusion-ADSK:Project/Folder/ -vv 2>&1 | grep -E "(Put|upload|storage|version)"

# Trace path resolution
rclone ls Fusion-ADSK:Project/Deep/Nested/Path/ -vv 2>&1 | grep resolvePath

# Trace cache behavior
rclone ls Fusion-ADSK: -vv 2>&1 | grep -E "(cache|invalidate|replaceChildren)"

# Trace rate limiting
rclone ls Fusion-ADSK: -vv 2>&1 | grep throttle
```
