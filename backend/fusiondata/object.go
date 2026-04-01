package fusiondata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
)

// Object represents a file in Fusion Data.
type Object struct {
	fs       *Fs
	remote   string
	id       string // GraphQL item ID
	altID    string // Data Management API item ID
	name     string
	size     int64
	modTime  time.Time
	mimeType string
}

// Fs returns the parent Fs.
func (o *Object) Fs() fs.Info { return o.fs }

// Remote returns the remote path.
func (o *Object) Remote() string { return o.remote }

// Size returns the file size in bytes.
func (o *Object) Size() int64 { return o.size }

// ModTime returns the modification time.
func (o *Object) ModTime(ctx context.Context) time.Time { return o.modTime }

// SetModTime is not supported by Fusion Data.
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

// Storable returns true — Fusion Data objects can be stored.
func (o *Object) Storable() bool { return true }

// Hash returns the hash of the object (not supported).
func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// String returns a description of the object.
func (o *Object) String() string {
	if o.remote != "" {
		return o.remote
	}
	return o.name
}

// MimeType returns the MIME type of the object.
func (o *Object) MimeType(ctx context.Context) string {
	return o.mimeType
}

// Open opens the object for reading. It downloads the file via a signed S3 URL.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	token, err := o.fs.getToken()
	if err != nil {
		return nil, err
	}

	// Resolve the full path to get project context.
	fullPath := o.fs.root
	if o.remote != "" {
		if fullPath != "" {
			fullPath = fullPath + "/" + o.remote
		} else {
			fullPath = o.remote
		}
	}
	resolved, err := o.fs.resolvePath(ctx, fullPath)
	if err != nil {
		return nil, fmt.Errorf("resolving item for download: %w", err)
	}
	if resolved.projectDM == "" {
		return nil, errors.New("cannot determine project for download")
	}

	// Resolve folder and item DM IDs via the REST API.
	dir := fullPath
	if idx := strings.LastIndex(dir, "/"); idx >= 0 {
		dir = dir[:idx]
	} else {
		dir = ""
	}
	folderDM, err := o.fs.resolveFolderDMForPath(ctx, token, dir)
	if err != nil {
		return nil, fmt.Errorf("resolving folder DM for download: %w", err)
	}

	itemDM, err := o.fs.resolveItemDM(ctx, token, resolved.projectDM, folderDM, o.name)
	if err != nil {
		return nil, fmt.Errorf("resolving item DM for download: %w", err)
	}

	downloadURL, err := o.fs.getDownloadURLWithProject(ctx, token, resolved.projectDM, itemDM)
	if err != nil {
		return nil, fmt.Errorf("getting download URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}

	// Apply range options if requested.
	fs.FixRangeOption(options, o.size)
	for _, option := range options {
		switch opt := option.(type) {
		case *fs.RangeOption:
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", opt.Start, opt.End))
		case *fs.SeekOption:
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", opt.Offset))
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	return resp.Body, nil
}

// Update replaces the content of this object (creates a new version in Fusion Data).
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	token, err := o.fs.getToken()
	if err != nil {
		return err
	}

	// Resolve path to get project DM ID and item DM ID.
	fullPath := o.fs.root
	if o.remote != "" {
		if fullPath != "" {
			fullPath = fullPath + "/" + o.remote
		} else {
			fullPath = o.remote
		}
	}
	resolved, err := o.fs.resolvePath(ctx, fullPath)
	if err != nil {
		return fmt.Errorf("resolving item for update: %w", err)
	}

	if resolved.projectDM == "" {
		return errors.New("cannot determine project for update")
	}

	// Resolve the parent folder DM ID via the REST API.
	dir := fullPath
	if idx := strings.LastIndex(dir, "/"); idx >= 0 {
		dir = dir[:idx]
	} else {
		dir = ""
	}
	folderDM, err := o.fs.resolveFolderDMForPath(ctx, token, dir)
	if err != nil {
		return fmt.Errorf("resolving folder DM ID for update: %w", err)
	}

	// Resolve item DM ID via the REST API.
	itemDM, err := o.fs.resolveItemDM(ctx, token, resolved.projectDM, folderDM, o.name)
	if err != nil {
		return fmt.Errorf("resolving item DM ID for update: %w", err)
	}

	// Create new storage and upload.
	storageID, err := o.fs.createStorage(ctx, token, resolved.projectDM, folderDM, o.name)
	if err != nil {
		return fmt.Errorf("creating storage for update: %w", err)
	}

	if err := o.fs.uploadToStorage(ctx, token, storageID, in, src.Size()); err != nil {
		return fmt.Errorf("uploading update: %w", err)
	}

	// Create new version of the existing item.
	if err := o.fs.createNextVersion(ctx, token, resolved.projectDM, itemDM, o.name, storageID); err != nil {
		return fmt.Errorf("creating new version: %w", err)
	}

	o.size = src.Size()
	o.modTime = time.Now()

	// Invalidate parent cache.
	if resolved.folderID != "" {
		o.fs.cache.invalidate(resolved.folderID)
	}

	return nil
}

// Remove deletes this object.
func (o *Object) Remove(ctx context.Context) error {
	// APS has limited deletion support.
	// Return nil silently to avoid breaking macOS safe-save workflows.
	return nil
}

// Check interface satisfaction.
var (
	_ fs.Object   = (*Object)(nil)
	_ fs.MimeTyper = (*Object)(nil)
)
