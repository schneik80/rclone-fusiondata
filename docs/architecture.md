# Architecture

This document describes the architecture of the rclone-fusiondata backend using C4 model diagrams.

## C4 Level 1: System Context

How the rclone-fusiondata backend fits into the broader system.

```mermaid
C4Context
    title System Context - rclone-fusiondata

    Person(user, "User", "Engineer using Fusion Data")

    System(rclone, "rclone-fusiondata", "Custom rclone binary with Fusion Data backend")

    System_Ext(aps_gql, "APS GraphQL API", "Manufacturing Data Model<br/>/mfg/graphql")
    System_Ext(aps_dm, "APS Data Management API", "REST API for files<br/>/data/v1/...")
    System_Ext(aps_oss, "APS Object Storage (S3)", "Signed URL upload/download<br/>/oss/v2/...")
    System_Ext(aps_auth, "APS OAuth2", "Authentication<br/>/authentication/v2/...")
    System_Ext(fusion_web, "Fusion Team Web", "Browser-based file management")

    Rel(user, rclone, "mount, ls, copy, sync")
    Rel(rclone, aps_auth, "OAuth2 PKCE login + token refresh")
    Rel(rclone, aps_gql, "List hubs, projects, folders, items")
    Rel(rclone, aps_dm, "Create storage, versions, folders")
    Rel(rclone, aps_oss, "Upload/download via signed S3 URLs")
    Rel(user, fusion_web, "Also manages files via browser")
    Rel(fusion_web, aps_dm, "Same backend APIs")
```

## C4 Level 2: Container Diagram

The major components inside the rclone-fusiondata binary.

```mermaid
C4Container
    title Container Diagram - rclone-fusiondata Backend

    Person(user, "User")

    System_Boundary(rclone_bin, "rclone binary") {
        Container(vfs, "VFS Layer", "rclone/vfs", "FUSE mount, caching, dir-cache-time, poll-interval")
        Container(backend, "fusiondata Backend", "Go package", "Implements fs.Fs, fs.Object, fs.ChangeNotifier")
        Container(graphql_client, "GraphQL Client", "graphql.go", "Query execution, rate limiting, retry, throttle")
        Container(rest_client, "REST Client", "rest.go", "DM API calls, S3 upload/download, doAPIRequest")
        Container(path_resolver, "Path Resolver", "path.go", "Translates rclone paths to Fusion API IDs")
        Container(cache, "Path Cache", "cache.go", "TTL-based name-to-ID cache")
        Container(oauth, "OAuth2 + Token Persistence", "fusiondata.go", "PKCE flow, persistingTokenSource")
    }

    System_Ext(aps_gql, "APS GraphQL API")
    System_Ext(aps_dm, "APS Data Management API")
    System_Ext(aps_s3, "S3 Signed URLs")

    Rel(user, vfs, "FUSE mount / CLI commands")
    Rel(vfs, backend, "fs.Fs interface calls")
    Rel(backend, graphql_client, "List operations")
    Rel(backend, rest_client, "Write operations, downloads")
    Rel(backend, path_resolver, "Resolve paths to IDs")
    Rel(path_resolver, cache, "Check/populate cache")
    Rel(path_resolver, graphql_client, "Fetch children on cache miss")
    Rel(graphql_client, aps_gql, "GraphQL over HTTPS")
    Rel(rest_client, aps_dm, "JSON:API over HTTPS")
    Rel(rest_client, aps_s3, "PUT/GET signed URLs")
    Rel(oauth, aps_dm, "Bearer token injection")
```

## C4 Level 3: Component Diagram

Detailed view of the fusiondata backend package.

```mermaid
C4Component
    title Component Diagram - backend/fusiondata Package

    Container_Boundary(pkg, "backend/fusiondata") {

        Component(fs_impl, "Fs struct", "fusiondata.go", "Implements fs.Fs:<br/>List, Put, Mkdir, Rmdir,<br/>NewObject, ChangeNotify")

        Component(obj_impl, "Object struct", "object.go", "Implements fs.Object:<br/>Open, Update, Remove,<br/>Size, ModTime, MimeType")

        Component(config, "Config Handler", "fusiondata.go", "OAuth2 PKCE flow,<br/>hub selection state machine,<br/>persistingTokenSource")

        Component(gql, "gqlQuery", "graphql.go", "GraphQL execution with<br/>throttle + retry")

        Component(throttle_comp, "apiThrottle", "graphql.go", "Rate limiter ticker +<br/>semaphore (max 10)")

        Component(queries, "Query Functions", "queries.go", "GetHubs, GetProjects,<br/>GetFolders, GetItems,<br/>GetItemDetails")

        Component(rest, "REST Functions", "rest.go", "createStorage, uploadToStorage,<br/>createFirstVersion,<br/>createNextVersion,<br/>getDownloadURLWithProject")

        Component(do_api, "doAPIRequest", "rest.go", "HTTP execution with<br/>throttle + retry +<br/>error classification")

        Component(path, "resolvePath", "path.go", "Path segment walker,<br/>temp file detection")

        Component(cache_comp, "pathCache", "cache.go", "TTL entries,<br/>replaceChildren,<br/>invalidate")

        Component(change, "ChangeNotify", "fusiondata.go", "Polls directories,<br/>compares snapshots,<br/>notifies VFS of changes")
    }

    Rel(fs_impl, queries, "List calls")
    Rel(fs_impl, rest, "Put/Mkdir calls")
    Rel(fs_impl, path, "Resolve paths")
    Rel(fs_impl, change, "Starts poller goroutine")
    Rel(obj_impl, rest, "Open/Update calls")
    Rel(obj_impl, path, "Resolve paths")
    Rel(queries, gql, "Execute queries")
    Rel(gql, throttle_comp, "Rate limit + semaphore")
    Rel(rest, do_api, "Execute requests")
    Rel(do_api, throttle_comp, "Rate limit + semaphore")
    Rel(path, cache_comp, "Check/populate")
    Rel(path, queries, "Fetch on cache miss")
    Rel(change, fs_impl, "Calls List() to poll")
```

## Data Model: Fusion Data Hierarchy

```mermaid
graph TD
    Hub["Hub<br/><i>Organization/Team</i>"]
    Project["Project<br/><i>Top-level container</i>"]
    RootFolder["Root Folder<br/><i>Invisible in GraphQL</i><br/><i>e.g. 'Project Files'</i>"]
    Folder["Folder<br/><i>User-visible directory</i>"]
    SubFolder["Sub Folder"]
    Item["Item<br/><i>DesignItem, DrawingItem,<br/>BasicItem, etc.</i>"]
    Version["Version<br/><i>Immutable snapshot</i>"]
    Storage["Storage Object<br/><i>S3 bucket/key</i>"]

    Hub --> Project
    Project --> RootFolder
    RootFolder --> Folder
    RootFolder --> Item
    Folder --> SubFolder
    Folder --> Item
    SubFolder --> Item
    Item --> Version
    Version --> Storage

    style RootFolder fill:#ff9,stroke:#aa0
    style Storage fill:#9df,stroke:#06a
```

## API Mapping

| rclone Operation | API Used | Endpoint |
|---|---|---|
| List projects | GraphQL | `hub.projects` |
| List folders | GraphQL | `foldersByProject` |
| List items | GraphQL | `itemsByFolder` / `itemsByProject` |
| Item details | GraphQL | `item(hubId, itemId)` |
| Download file | REST + S3 | `GET .../items/.../tip` then `GET signeds3download` |
| Upload file | REST + S3 | `POST .../storage` then `GET signeds3upload` then `PUT` S3 |
| Create version | REST | `POST .../items` or `POST .../versions` |
| Create folder | REST | `POST .../folders` |
| Resolve folder DM IDs | REST | `GET .../topFolders` + `GET .../folders/.../contents` |

## Dual-ID System

Fusion Data uses two ID systems that the backend must bridge:

| System | Used For | Format |
|---|---|---|
| **GraphQL ID** | Listing, navigation, item details | Opaque string (e.g., `a]W5...`) |
| **DM API ID** | Upload, download, create, version | URN (e.g., `urn:adsk.wipprod:dm.lineage:XXXX`) |

- **Hubs and Projects** expose `alternativeIdentifiers` in GraphQL that provide the DM API ID
- **Folders** do NOT expose DM IDs in GraphQL; the backend resolves them by walking the REST `topFolders` and `contents` endpoints
- **Items** also lack DM IDs in GraphQL; resolved via REST `contents` endpoint by name matching
