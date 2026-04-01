package fusiondata

import (
	"context"
	"fmt"
	"strings"

	"github.com/rclone/rclone/fs"
)

// resolvedPath holds the result of resolving an rclone path to Fusion API IDs.
type resolvedPath struct {
	kind      string // "hub", "project", "folder", "item"
	id        string // GraphQL ID
	altID     string // Data Management API ID
	projectID string // parent project GraphQL ID
	projectDM string // parent project DM API ID
	folderID  string // parent folder GraphQL ID (for items in folders)
	folderDM  string // parent folder DM API ID (for items in folders)
}

// splitPath splits a path into non-empty segments.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// resolvePath translates an rclone path into the Fusion API IDs needed for operations.
//
// Path structure (with hub as configured root):
//
//	""                           -> hub
//	"ProjectName"                -> project
//	"ProjectName/Folder"         -> folder (top-level)
//	"ProjectName/Folder/Sub"     -> folder (nested)
//	"ProjectName/Folder/file.f3d"-> item
func (f *Fs) resolvePath(ctx context.Context, p string) (*resolvedPath, error) {
	segments := splitPath(p)

	if len(segments) == 0 {
		return &resolvedPath{kind: "hub", id: f.hubID}, nil
	}

	// If any path segment is a macOS temp name, return not-found.
	// Mkdir/Rmdir/Put/Remove handle temp paths separately.
	for _, seg := range segments {
		if isTempName(seg) {
			return nil, fs.ErrorObjectNotFound
		}
	}

	token, err := f.getToken()
	if err != nil {
		return nil, err
	}

	// Segment 0: find project by name.
	project, err := f.findChildByName(ctx, token, f.hubID, "hub", segments[0])
	if err != nil {
		return nil, fmt.Errorf("project %q not found: %w", segments[0], err)
	}

	if len(segments) == 1 {
		return &resolvedPath{
			kind:      "project",
			id:        project.ID,
			altID:     project.AltID,
			projectID: project.ID,
			projectDM: project.AltID,
		}, nil
	}

	// Segments 1..n: walk folders/items.
	var currentID = project.ID
	var currentKind = "project"
	var currentFolderDM string
	projectID := project.ID
	projectDM := project.AltID

	for i := 1; i < len(segments); i++ {
		child, err := f.findChildByName(ctx, token, currentID, currentKind, segments[i])
		if err != nil {
			return nil, fmt.Errorf("path segment %q not found in %s: %w", segments[i], strings.Join(segments[:i], "/"), err)
		}

		if i == len(segments)-1 {
			// Last segment — return whatever it is.
			return &resolvedPath{
				kind:      child.Kind,
				id:        child.ID,
				altID:     child.AltID,
				projectID: projectID,
				projectDM: projectDM,
				folderID:  currentID,
				folderDM:  currentFolderDM,
			}, nil
		}

		// Not the last segment — must be a container.
		if !child.IsContainer {
			return nil, fmt.Errorf("%q is not a directory", strings.Join(segments[:i+1], "/"))
		}
		currentFolderDM = child.AltID
		currentID = child.ID
		currentKind = child.Kind
	}

	// Should not reach here, but just in case.
	return nil, fs.ErrorObjectNotFound
}

// findChildByName searches for a child with the given name under a parent.
// parentKind determines which API call to use.
func (f *Fs) findChildByName(ctx context.Context, token, parentID, parentKind, name string) (*NavItem, error) {
	// Check cache first.
	if cached := f.cache.getChild(parentID, name); cached != nil {
		return cached, nil
	}

	// Fetch children and populate cache.
	children, err := f.listChildren(ctx, token, parentID, parentKind)
	if err != nil {
		return nil, err
	}

	for i := range children {
		f.cache.putChild(parentID, children[i].Name, &children[i])
		if children[i].Name == name {
			return &children[i], nil
		}
	}

	return nil, fs.ErrorObjectNotFound
}

// listChildren returns all children of a parent node.
func (f *Fs) listChildren(ctx context.Context, token, parentID, parentKind string) ([]NavItem, error) {
	switch parentKind {
	case "hub":
		return GetProjects(ctx, token, f.hubID)
	case "project":
		// A project can contain both top-level folders and root items.
		folders, err := GetFolders(ctx, token, parentID)
		if err != nil {
			return nil, err
		}
		items, err := GetProjectItems(ctx, token, parentID)
		if err != nil {
			return nil, err
		}
		return append(folders, items...), nil
	case "folder":
		return GetItems(ctx, token, f.hubID, parentID)
	default:
		return nil, fmt.Errorf("cannot list children of %s", parentKind)
	}
}
