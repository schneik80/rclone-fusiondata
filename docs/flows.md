# Operation Flow Diagrams

Detailed sequence diagrams for each major operation in the fusiondata backend.

## Authentication Flow (rclone config)

```mermaid
sequenceDiagram
    participant User
    participant rclone as rclone config
    participant Browser
    participant Callback as localhost:7879
    participant APS as APS OAuth2

    User->>rclone: Create new fusiondata remote
    rclone->>User: Prompt for Client ID
    User->>rclone: Enter Client ID

    Note over rclone: Generate PKCE verifier + challenge

    rclone->>Browser: Open authorization URL
    Browser->>APS: GET /authentication/v2/authorize
    APS->>Browser: Login page
    User->>Browser: Authenticate + consent
    APS->>Callback: Redirect with authorization code
    Callback->>rclone: Return code

    rclone->>APS: POST /authentication/v2/token<br/>(code + PKCE verifier)
    APS->>rclone: access_token + refresh_token

    Note over rclone: Save token to rclone config

    rclone->>APS: GraphQL: GetHubs
    APS->>rclone: List of hubs
    rclone->>User: Choose a hub
    User->>rclone: Select hub

    Note over rclone: Save hub_id + hub_name to config
    rclone->>User: Config complete
```

## List Directory (rclone ls)

```mermaid
sequenceDiagram
    participant VFS as rclone VFS
    participant Fs as Fs.List()
    participant Path as resolvePath
    participant Cache as pathCache
    participant GQL as GraphQL API

    VFS->>Fs: List(ctx, "Project/Folder")
    Fs->>Path: resolvePath("Project/Folder")

    Path->>Cache: getChild(hubID, "Project")
    alt Cache hit
        Cache->>Path: NavItem (project)
    else Cache miss
        Path->>GQL: GetProjects(hubID)
        GQL->>Path: []NavItem
        Path->>Cache: putChild for each
    end

    Path->>Cache: getChild(projectID, "Folder")
    alt Cache hit
        Cache->>Path: NavItem (folder)
    else Cache miss
        Path->>GQL: GetFolders(projectID) + GetProjectItems(projectID)
        GQL->>Path: []NavItem
        Path->>Cache: putChild for each
    end

    Path->>Fs: resolvedPath{kind:"folder", id:"..."}

    Fs->>GQL: GetItems(hubID, folderID)
    GQL->>Fs: []NavItem (with size, modTime, mimeType)

    Note over Fs: Apply Fusion extensions:<br/>DesignItem → .fusiondesign<br/>DrawingItem → .fusiondrawing<br/>etc. (by typename or MIME fallback)

    Fs->>Cache: replaceChildren(folderID, items)
    Fs->>VFS: []DirEntries (dirs + objects with extensions)
```

## Download File (Open)

```mermaid
sequenceDiagram
    participant VFS as rclone VFS
    participant Obj as Object.Open()
    participant Path as resolvePath
    participant REST as REST DM API
    participant S3 as S3 Signed URL

    VFS->>Obj: Open(ctx, options)
    Obj->>Path: resolvePath(fullPath)
    Path->>Obj: resolvedPath{projectDM:"..."}

    Note over Obj: Resolve folder DM ID via REST

    Obj->>REST: getTopFolders(hubDM, projectDM)
    REST->>Obj: top folders
    Obj->>REST: getFolderContents(projectDM, topFolderDM)
    REST->>Obj: folder entries (find target folder DM ID)

    Note over Obj: Resolve item DM ID via REST

    Obj->>REST: getFolderContents(projectDM, folderDM)
    REST->>Obj: items (find target item DM ID)

    Note over Obj: Get signed download URL

    Obj->>REST: GET /data/v1/projects/.../items/.../tip
    REST->>Obj: version with storage URN

    Obj->>REST: GET /oss/v2/buckets/.../signeds3download
    REST->>Obj: signed S3 URL

    Obj->>S3: GET signed URL
    S3->>Obj: file content stream

    Note over Obj: Capture ETag header

    Obj->>VFS: io.ReadCloser
```

## Upload New File (Put)

```mermaid
sequenceDiagram
    participant VFS as rclone VFS
    participant Fs as Fs.Put()
    participant Path as resolvePath
    participant REST as REST DM API
    participant S3 as S3 Signed URL

    VFS->>Fs: Put(ctx, reader, srcInfo)

    Note over Fs: Check for temp filename (.sb-*)<br/>Skip if temp

    Fs->>Path: resolvePath(parentDir)
    Path->>Fs: resolvedPath{projectDM:"..."}

    Note over Fs: Resolve folder DM ID via REST

    Fs->>REST: resolveFolderDMPath(folderNames)
    REST->>Fs: folderDM

    Note over Fs: Create storage object

    Fs->>REST: POST /data/v1/projects/.../storage
    REST->>Fs: storageID (URN)

    alt File size <= 100MB
        Note over Fs: Single-part upload
        Fs->>REST: GET /oss/v2/.../signeds3upload
        REST->>Fs: signed URL + uploadKey
        Fs->>S3: PUT signed URL (file content)
        S3->>Fs: 200 OK
        Fs->>REST: POST /oss/v2/.../signeds3upload (complete)
    else File size > 100MB
        Note over Fs: Multipart upload
        Fs->>REST: GET /oss/v2/.../signeds3upload?parts=N
        REST->>Fs: N signed URLs + uploadKey
        loop Each part
            Fs->>S3: PUT signed URL (chunk)
            S3->>Fs: 200 OK + ETag
        end
        Fs->>REST: POST /oss/v2/.../signeds3upload (complete with ETags)
    end

    Note over Fs: Check if file already exists

    alt File exists
        Fs->>REST: resolveItemDM (find DM item ID)
        Fs->>REST: POST /data/v1/projects/.../versions (new version)
    else New file
        Fs->>REST: POST /data/v1/projects/.../items (first version)
    end

    Fs->>VFS: Object
```

## Update Existing File (creates new version)

```mermaid
sequenceDiagram
    participant VFS as rclone VFS
    participant Obj as Object.Update()
    participant REST as REST DM API
    participant S3 as S3 Signed URL

    VFS->>Obj: Update(ctx, reader, srcInfo)

    Note over Obj: Resolve project + folder + item DM IDs

    Obj->>REST: resolveFolderDMForPath()
    REST->>Obj: folderDM
    Obj->>REST: resolveItemDM(projectDM, folderDM, name)
    REST->>Obj: itemDM

    Note over Obj: Upload new content

    Obj->>REST: createStorage(projectDM, folderDM, name)
    REST->>Obj: storageID
    Obj->>REST: uploadToStorage(storageID, reader, size)
    REST->>S3: PUT signed URL
    S3->>REST: 200 OK

    Note over Obj: Create new version

    Obj->>REST: POST /data/v1/projects/.../versions
    REST->>Obj: 201 Created

    Note over Obj: Invalidate parent cache
    Obj->>Obj: cache.invalidate(folderID)
```

## Change Notification Polling (ChangeNotify)

```mermaid
sequenceDiagram
    participant VFS as rclone VFS
    participant CN as ChangeNotify goroutine
    participant Fs as Fs.List()
    participant GQL as GraphQL API

    VFS->>CN: Start polling (interval from --poll-interval)

    loop Every poll interval
        CN->>Fs: List(ctx, "") — list projects
        Fs->>GQL: GetProjects
        GQL->>Fs: current projects
        Fs->>CN: DirEntries

        Note over CN: Compare with previous snapshot

        alt New/modified/deleted entries
            CN->>VFS: notifyFunc(path, EntryType)
            Note over VFS: Invalidates dir cache for path
        end

        CN->>CN: Update snapshot

        loop Each known project
            CN->>Fs: List(ctx, projectName)
            Fs->>GQL: GetFolders + GetProjectItems
            GQL->>Fs: current entries
            Fs->>CN: DirEntries

            Note over CN: Compare with snapshot, notify changes
        end
    end
```

## Rate Limiting and Throttle

```mermaid
flowchart TD
    A[API Call] --> B{Throttle initialized?}
    B -->|No| F[Execute request]
    B -->|Yes| C[Acquire semaphore slot<br/>max 10 concurrent]
    C --> D[Wait for rate limiter tick<br/>1/rate_limit seconds]
    D --> F
    F --> G{Response status?}
    G -->|200-299| H[Return response]
    G -->|429| I[Read Retry-After header]
    I --> J[Backoff delay]
    J --> K{Retries remaining?}
    K -->|Yes| C
    K -->|No| L[Return error]
    G -->|5xx| J
    G -->|401| M[Return PermissionDenied]
    G -->|404| N[Return ObjectNotFound]
    H --> O[Release semaphore]
    L --> O

    style C fill:#ff9,stroke:#aa0
    style D fill:#ff9,stroke:#aa0
    style I fill:#f99,stroke:#a00
```

## Path Resolution

```mermaid
flowchart TD
    A["resolvePath('Project/Folder/file.f3d')"] --> B[Split into segments]
    B --> C{Any segment is temp name?}
    C -->|Yes| D[Return ErrorObjectNotFound]
    C -->|No| E[Segment 0: Find project by name]

    E --> F{Cache hit?}
    F -->|Yes| G[Use cached NavItem]
    F -->|No| H[GetProjects from GraphQL]
    H --> I[Populate cache]
    I --> G

    G --> J{More segments?}
    J -->|No| K[Return project resolvedPath]
    J -->|Yes| L[Segment N: Find child by name]

    L --> M{Cache hit?}
    M -->|Yes| N[Use cached NavItem]
    M -->|No| O[listChildren from GraphQL]
    O --> P[Populate cache]
    P --> N

    N --> Q{Last segment?}
    Q -->|Yes| R[Return resolvedPath<br/>kind=folder/item]
    Q -->|No| S{Is container?}
    S -->|Yes| L
    S -->|No| T[Error: not a directory]
```
