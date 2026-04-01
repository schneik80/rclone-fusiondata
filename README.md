# rclone-fusiondata

An [rclone](https://rclone.org) backend for [Autodesk Fusion Data (APS)](https://aps.autodesk.com) that lets you mount your Fusion Team hub as a network drive.

Browse, download, upload, and sync Fusion Data projects, folders, and files using standard rclone commands or as a mounted FUSE filesystem.

## Features

- **OAuth2 PKCE authentication** with interactive hub selection during setup
- **Mount as a network drive** via `rclone mount` (requires [macFUSE](https://osxfuse.github.io/) on macOS)
- **List, download, upload** files with automatic versioning (new uploads create new versions)
- **Create folders** via Finder or CLI
- **Chunked multipart upload** for large files (>100MB)
- **Rate limiting** with configurable requests/second and concurrency cap
- **Retry with exponential backoff** on API throttling (429) and server errors (5xx)
- **Token persistence** — refreshed OAuth tokens saved back to config automatically
- **Server-side change detection** via `--poll-interval` (ChangeNotify interface)
- **Configurable cache TTL** aligned with rclone's `--dir-cache-time`
- **Comprehensive logging** at Debug/Info/Notice/Error levels
- **Fusion file type extensions** — DesignItem, DrawingItem, etc. display with custom extensions (`.fusiondesign`, `.fusiondrawing`, etc.)

## Prerequisites

- **Go 1.22+** (for building from source)
- **macFUSE** (for `rclone mount` on macOS): `brew install macfuse`
- **Autodesk account** with Fusion Team hub access
- **APS app registration** at [aps.autodesk.com](https://aps.autodesk.com) with:
  - Redirect URI: `http://localhost:7879/callback`
  - Scopes: `data:read data:write data:create user-profile:read`

## Quick Start

### Build

```bash
git clone https://github.com/schneik80/rclone-fusiondata.git
cd rclone-fusiondata
make build
```

### Configure

```bash
./rclone config
# Choose "n" for new remote
# Name it (e.g., "fusion")
# Select "fusiondata" from the backend list
# Enter your APS Client ID
# (Optional) Enter Client Secret for confidential apps
# Authenticate in browser
# Select your hub
```

### Use

```bash
# List projects
./rclone lsd fusion:

# List files in a project folder
./rclone ls fusion:MyProject/Designs/

# Download a file
./rclone copy fusion:MyProject/Designs/Part.f3d ./local/

# Upload a file (creates new item or new version if exists)
./rclone copy ./local/file.pdf fusion:MyProject/Designs/

# Create a folder
./rclone mkdir fusion:MyProject/NewFolder

# Mount as network drive
./rclone mount fusion: ~/FusionData --vfs-cache-mode full
```

### Recommended Mount Command

```bash
./rclone mount fusion: ~/FusionData \
  --vfs-cache-mode full \
  --dir-cache-time 2m \
  --poll-interval 1m \
  --fusiondata-cache-ttl 2m \
  --fusiondata-rate-limit 5 \
  --rc
```

## Configuration Options

| Flag | Default | Description |
|------|---------|-------------|
| `--fusiondata-client-id` | *(required)* | APS OAuth2 Client ID |
| `--fusiondata-client-secret` | *(empty)* | APS OAuth2 Client Secret (for confidential apps) |
| `--fusiondata-region` | `US` | APS region: `US`, `EMEA`, or `AUS` |
| `--fusiondata-cache-ttl` | `5m` | Internal path cache TTL (align with `--dir-cache-time`) |
| `--fusiondata-rate-limit` | `5` | Max API requests per second |
| `--fusiondata-upload-chunk-size` | `100Mi` | Chunk size for multipart uploads |

## File Type Extensions

Fusion Data items are displayed with custom file extensions based on their type. This makes it easy to identify Fusion-specific files in Finder and file managers.

| Fusion Type | Extension | Detection |
|---|---|---|
| DesignItem | `.fusiondesign` | `__typename` or MIME `application/vnd.autodesk.fusion360` |
| ConfiguredDesignItem | `.fusionconfig` | `__typename` or MIME `application/vnd.autodesk.fusionconfig` |
| DrawingItem | `.fusiondrawing` | `__typename` or MIME `application/vnd.autodesk.fusiondrawing` |
| DrawingTemplateItem | `.drawingtemplate` | `__typename` or MIME `application/vnd.autodesk.fusiondrawingtemplate` |
| BasicItem (PDF, PNG, etc.) | *(unchanged)* | Original extension preserved |
| Folder | *(none)* | Displayed as directory |

**Examples:**
```
MyProject/
  Designs/
    Engine Block.fusiondesign          (DesignItem)
    Assembly Drawing.fusiondrawing     (DrawingItem)
    My Template.drawingtemplate        (DrawingTemplateItem)
    reference.pdf                      (BasicItem — unchanged)
    photo.png                          (BasicItem — unchanged)
```

Extensions are preserved on upload — if you save `Part.fusiondesign`, that full name is sent to the API.

## Debugging

Use rclone's standard verbosity flags:

```bash
# Info level — operations, retries, transfer counts
./rclone ls fusion: -v

# Debug level — full API call trace, cache hits/misses, path resolution
./rclone ls fusion: -vv

# Log to file
./rclone mount fusion: ~/FusionData --vfs-cache-mode full -vv --log-file=/tmp/fusiondata.log
```

See [docs/debugging.md](docs/debugging.md) for the full troubleshooting guide.

## Architecture

The backend uses two Autodesk APIs:

- **GraphQL API** (`/mfg/graphql`) for read operations: listing hubs, projects, folders, items
- **Data Management REST API** (`/data/v1/...`) for write operations: upload, download, create folders, versioning

See [docs/architecture.md](docs/architecture.md) for C4 diagrams and detailed architecture.
See [docs/flows.md](docs/flows.md) for operation flow diagrams.

## Known Limitations

- **No SetModTime** — APS sets modification time automatically; rclone uses size-based comparison for sync
- **No content hash** — S3 ETags are captured but not exposed to rclone's hash system yet
- **No permanent delete** — `Remove` and `Rmdir` silently succeed but don't delete server-side
- **Duplicate filenames** — Fusion allows them; the backend uses the first match
- **Token expiry** — Public PKCE refresh tokens expire after 14 days of non-use; run `rclone config reconnect` to re-authenticate

## Project Structure

```
rclone-fusiondata/
  rclone.go                       # Entry point (imports rclone + custom backend)
  go.mod / go.sum                 # Go module
  Makefile                        # Build targets
  backend/fusiondata/
    fusiondata.go                 # Registration, OAuth, NewFs, List, Put, Mkdir, ChangeNotify
    graphql.go                    # GraphQL client, rate limiter, throttle, retry
    queries.go                    # Hub/project/folder/item GraphQL queries
    rest.go                       # REST API: upload, download, versioning, folders
    object.go                     # fs.Object: Open, Update, Remove
    path.go                       # Path resolution: rclone paths -> Fusion API IDs
    cache.go                      # TTL-based name-to-ID cache
  docs/
    architecture.md               # C4 diagrams
    flows.md                      # Operation flow diagrams
    debugging.md                  # Logging and troubleshooting
```

## License

MIT
