package fusiondata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// GetHubs
// ---------------------------------------------------------------------------

// GetHubs returns all hubs accessible to the authenticated user.
// Uses the provided HTTP client (oauth2 auto-injects tokens).
func GetHubs(ctx context.Context, client *http.Client) ([]NavItem, error) {
	const qFirst = `
		query GetHubs {
			hubs(pagination: { limit: 50 }) {
				pagination { cursor }
				results {
					id name fusionWebUrl
					alternativeIdentifiers { dataManagementAPIHubId }
				}
			}
		}`
	const qNext = `
		query GetHubsNext($cursor: String!) {
			hubs(pagination: { cursor: $cursor, limit: 50 }) {
				pagination { cursor }
				results {
					id name fusionWebUrl
					alternativeIdentifiers { dataManagementAPIHubId }
				}
			}
		}`

	type hubResult struct {
		ID                     string `json:"id"`
		Name                   string `json:"name"`
		FusionWebURL           string `json:"fusionWebUrl"`
		AlternativeIdentifiers struct {
			DataManagementAPIHubID string `json:"dataManagementAPIHubId"`
		} `json:"alternativeIdentifiers"`
	}

	pages, err := allPages(ctx, client, qFirst, qNext, nil, func(data json.RawMessage) (pageResult, error) {
		var r struct {
			Hubs struct {
				Pagination struct{ Cursor string `json:"cursor"` } `json:"pagination"`
				Results    []hubResult                             `json:"results"`
			} `json:"hubs"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return pageResult{}, fmt.Errorf("hubs: %w", err)
		}
		raw, _ := json.Marshal(r.Hubs.Results)
		return pageResult{cursor: r.Hubs.Pagination.Cursor, data: raw}, nil
	})
	if err != nil {
		return nil, err
	}

	var all []hubResult
	for _, p := range pages {
		var batch []hubResult
		if err := json.Unmarshal(p, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}

	items := make([]NavItem, len(all))
	for i, h := range all {
		items[i] = NavItem{
			ID:          h.ID,
			Name:        h.Name,
			Kind:        "hub",
			AltID:       h.AlternativeIdentifiers.DataManagementAPIHubID,
			WebURL:      h.FusionWebURL,
			IsContainer: true,
		}
	}
	return items, nil
}

// GetHubsWithToken is a convenience wrapper for config, which has no Fs yet.
func GetHubsWithToken(ctx context.Context, token string) ([]NavItem, error) {
	const qFirst = `
		query GetHubs {
			hubs(pagination: { limit: 50 }) {
				pagination { cursor }
				results {
					id name fusionWebUrl
					alternativeIdentifiers { dataManagementAPIHubId }
				}
			}
		}`
	const qNext = `
		query GetHubsNext($cursor: String!) {
			hubs(pagination: { cursor: $cursor, limit: 50 }) {
				pagination { cursor }
				results {
					id name fusionWebUrl
					alternativeIdentifiers { dataManagementAPIHubId }
				}
			}
		}`

	type hubResult struct {
		ID                     string `json:"id"`
		Name                   string `json:"name"`
		FusionWebURL           string `json:"fusionWebUrl"`
		AlternativeIdentifiers struct {
			DataManagementAPIHubID string `json:"dataManagementAPIHubId"`
		} `json:"alternativeIdentifiers"`
	}

	pages, err := allPagesWithToken(ctx, token, qFirst, qNext, nil, func(data json.RawMessage) (pageResult, error) {
		var r struct {
			Hubs struct {
				Pagination struct{ Cursor string `json:"cursor"` } `json:"pagination"`
				Results    []hubResult                             `json:"results"`
			} `json:"hubs"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return pageResult{}, fmt.Errorf("hubs: %w", err)
		}
		raw, _ := json.Marshal(r.Hubs.Results)
		return pageResult{cursor: r.Hubs.Pagination.Cursor, data: raw}, nil
	})
	if err != nil {
		return nil, err
	}

	var all []hubResult
	for _, p := range pages {
		var batch []hubResult
		if err := json.Unmarshal(p, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}

	items := make([]NavItem, len(all))
	for i, h := range all {
		items[i] = NavItem{
			ID:          h.ID,
			Name:        h.Name,
			Kind:        "hub",
			AltID:       h.AlternativeIdentifiers.DataManagementAPIHubID,
			WebURL:      h.FusionWebURL,
			IsContainer: true,
		}
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// GetProjects
// ---------------------------------------------------------------------------

// GetProjects returns all active projects in a hub.
func GetProjects(ctx context.Context, client *http.Client, hubID string) ([]NavItem, error) {
	const qFirst = `
		query GetProjects($hubId: ID!) {
			hub(hubId: $hubId) {
				projects(pagination: { limit: 50 }) {
					pagination { cursor }
					results {
						id name fusionWebUrl projectStatus projectType
						alternativeIdentifiers { dataManagementAPIProjectId }
					}
				}
			}
		}`
	const qNext = `
		query GetProjectsNext($hubId: ID!, $cursor: String!) {
			hub(hubId: $hubId) {
				projects(pagination: { cursor: $cursor, limit: 50 }) {
					pagination { cursor }
					results {
						id name fusionWebUrl projectStatus projectType
						alternativeIdentifiers { dataManagementAPIProjectId }
					}
				}
			}
		}`

	type projectResult struct {
		ID                     string `json:"id"`
		Name                   string `json:"name"`
		FusionWebURL           string `json:"fusionWebUrl"`
		ProjectStatus          string `json:"projectStatus"`
		ProjectType            string `json:"projectType"`
		AlternativeIdentifiers struct {
			DataManagementAPIProjectID string `json:"dataManagementAPIProjectId"`
		} `json:"alternativeIdentifiers"`
	}

	pages, err := allPages(ctx, client, qFirst, qNext, map[string]any{"hubId": hubID}, func(data json.RawMessage) (pageResult, error) {
		var r struct {
			Hub struct {
				Projects struct {
					Pagination struct{ Cursor string `json:"cursor"` } `json:"pagination"`
					Results    []projectResult                         `json:"results"`
				} `json:"projects"`
			} `json:"hub"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return pageResult{}, fmt.Errorf("projects: %w", err)
		}
		raw, _ := json.Marshal(r.Hub.Projects.Results)
		return pageResult{cursor: r.Hub.Projects.Pagination.Cursor, data: raw}, nil
	})
	if err != nil {
		return nil, err
	}

	var all []projectResult
	for _, p := range pages {
		var batch []projectResult
		if err := json.Unmarshal(p, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}

	var items []NavItem
	for _, p := range all {
		if strings.EqualFold(p.ProjectStatus, "inactive") {
			continue
		}
		items = append(items, NavItem{
			ID:          p.ID,
			Name:        p.Name,
			Kind:        "project",
			AltID:       p.AlternativeIdentifiers.DataManagementAPIProjectID,
			WebURL:      p.FusionWebURL,
			IsContainer: true,
		})
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// GetFolders
// ---------------------------------------------------------------------------

// GetFolders returns the top-level folders in a project.
func GetFolders(ctx context.Context, client *http.Client, projectID string) ([]NavItem, error) {
	const qFirst = `
		query GetFolders($projectId: ID!) {
			foldersByProject(projectId: $projectId, pagination: { limit: 50 }) {
				pagination { cursor }
				results { id name }
			}
		}`
	const qNext = `
		query GetFoldersNext($projectId: ID!, $cursor: String!) {
			foldersByProject(projectId: $projectId, pagination: { cursor: $cursor, limit: 50 }) {
				pagination { cursor }
				results { id name }
			}
		}`

	type folderResult struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	pages, err := allPages(ctx, client, qFirst, qNext, map[string]any{"projectId": projectID}, func(data json.RawMessage) (pageResult, error) {
		var r struct {
			FoldersByProject struct {
				Pagination struct{ Cursor string `json:"cursor"` } `json:"pagination"`
				Results    []folderResult                         `json:"results"`
			} `json:"foldersByProject"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return pageResult{}, fmt.Errorf("folders: %w", err)
		}
		raw, _ := json.Marshal(r.FoldersByProject.Results)
		return pageResult{cursor: r.FoldersByProject.Pagination.Cursor, data: raw}, nil
	})
	if err != nil {
		return nil, err
	}

	var all []folderResult
	for _, p := range pages {
		var batch []folderResult
		if err := json.Unmarshal(p, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}

	items := make([]NavItem, len(all))
	for i, f := range all {
		items[i] = NavItem{
			ID:          f.ID,
			Name:        f.Name,
			Kind:        "folder",
			IsContainer: true,
		}
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// GetProjectItems
// ---------------------------------------------------------------------------

// GetProjectItems returns items at the root of a project (not in any folder).
func GetProjectItems(ctx context.Context, client *http.Client, projectID string) ([]NavItem, error) {
	const qFirst = `
		query GetProjectItems($projectId: ID!) {
			itemsByProject(projectId: $projectId, pagination: { limit: 50 }) {
				pagination { cursor }
				results {
					__typename id name
					size mimeType lastModifiedOn
				}
			}
		}`
	const qNext = `
		query GetProjectItemsNext($projectId: ID!, $cursor: String!) {
			itemsByProject(projectId: $projectId, pagination: { cursor: $cursor, limit: 50 }) {
				pagination { cursor }
				results {
					__typename id name
					size mimeType lastModifiedOn
				}
			}
		}`

	type itemResult struct {
		Typename   string `json:"__typename"`
		ID         string `json:"id"`
		Name       string `json:"name"`
		Size       string `json:"size"`
		MimeType   string `json:"mimeType"`
		ModifiedOn string `json:"lastModifiedOn"`
	}

	pages, err := allPages(ctx, client, qFirst, qNext, map[string]any{"projectId": projectID}, func(data json.RawMessage) (pageResult, error) {
		var r struct {
			ItemsByProject struct {
				Pagination struct{ Cursor string `json:"cursor"` } `json:"pagination"`
				Results    []itemResult                           `json:"results"`
			} `json:"itemsByProject"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return pageResult{}, fmt.Errorf("project items: %w", err)
		}
		raw, _ := json.Marshal(r.ItemsByProject.Results)
		return pageResult{cursor: r.ItemsByProject.Pagination.Cursor, data: raw}, nil
	})
	if err != nil {
		return nil, err
	}

	var all []itemResult
	for _, p := range pages {
		var batch []itemResult
		if err := json.Unmarshal(p, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}

	items := make([]NavItem, len(all))
	for i, it := range all {
		items[i] = navItemFromTypename(it.ID, it.Name, it.Typename)
		items[i].Size = parseSizeStr(it.Size)
		items[i].ModTime = parseTime(it.ModifiedOn)
		items[i].MimeType = it.MimeType
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// GetItems (by folder)
// ---------------------------------------------------------------------------

// GetItems returns items inside a folder.
func GetItems(ctx context.Context, client *http.Client, hubID, folderID string) ([]NavItem, error) {
	const qFirst = `
		query GetItems($hubId: ID!, $folderId: ID!) {
			itemsByFolder(hubId: $hubId, folderId: $folderId, pagination: { limit: 50 }) {
				pagination { cursor }
				results {
					__typename id name
					size mimeType lastModifiedOn
				}
			}
		}`
	const qNext = `
		query GetItemsNext($hubId: ID!, $folderId: ID!, $cursor: String!) {
			itemsByFolder(hubId: $hubId, folderId: $folderId, pagination: { cursor: $cursor, limit: 50 }) {
				pagination { cursor }
				results {
					__typename id name
					size mimeType lastModifiedOn
				}
			}
		}`

	type itemResult struct {
		Typename   string `json:"__typename"`
		ID         string `json:"id"`
		Name       string `json:"name"`
		Size       string `json:"size"`
		MimeType   string `json:"mimeType"`
		ModifiedOn string `json:"lastModifiedOn"`
	}

	pages, err := allPages(ctx, client, qFirst, qNext, map[string]any{"hubId": hubID, "folderId": folderID}, func(data json.RawMessage) (pageResult, error) {
		var r struct {
			ItemsByFolder struct {
				Pagination struct{ Cursor string `json:"cursor"` } `json:"pagination"`
				Results    []itemResult                          `json:"results"`
			} `json:"itemsByFolder"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return pageResult{}, fmt.Errorf("items: %w", err)
		}
		raw, _ := json.Marshal(r.ItemsByFolder.Results)
		return pageResult{cursor: r.ItemsByFolder.Pagination.Cursor, data: raw}, nil
	})
	if err != nil {
		return nil, err
	}

	var all []itemResult
	for _, p := range pages {
		var batch []itemResult
		if err := json.Unmarshal(p, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}

	items := make([]NavItem, len(all))
	for i, it := range all {
		items[i] = navItemFromTypename(it.ID, it.Name, it.Typename)
		items[i].Size = parseSizeStr(it.Size)
		items[i].ModTime = parseTime(it.ModifiedOn)
		items[i].MimeType = it.MimeType
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// GetItemDetails
// ---------------------------------------------------------------------------

// ItemDetails holds rich metadata for a single item.
type ItemDetails struct {
	ID            string
	Name          string
	Typename      string
	Size          string
	MimeType      string
	ExtensionType string
	FusionWebURL  string
	CreatedOn     time.Time
	CreatedBy     string
	ModifiedOn    time.Time
	ModifiedBy    string
	VersionNumber int
}

// GetItemDetails fetches rich metadata for a single item.
func GetItemDetails(ctx context.Context, client *http.Client, hubID, itemID string) (*ItemDetails, error) {
	const q = `
		query GetItemDetails($hubId: ID!, $itemId: ID!) {
			item(hubId: $hubId, itemId: $itemId) {
				__typename
				id
				name
				size
				mimeType
				extensionType
				createdOn
				createdBy  { firstName lastName }
				lastModifiedOn
				lastModifiedBy { firstName lastName }
				... on DesignItem {
					fusionWebUrl
					tipVersion { versionNumber }
				}
				... on DrawingItem {
					fusionWebUrl
					tipVersion { versionNumber }
				}
				... on ConfiguredDesignItem {
					fusionWebUrl
					tipVersion { versionNumber }
				}
			}
		}`

	data, err := gqlQuery(ctx, client, q, map[string]any{"hubId": hubID, "itemId": itemID})
	if err != nil {
		return nil, fmt.Errorf("item details: %w", err)
	}

	var raw struct {
		Item struct {
			Typename      string  `json:"__typename"`
			ID            string  `json:"id"`
			Name          string  `json:"name"`
			Size          string  `json:"size"`
			MimeType      string  `json:"mimeType"`
			ExtensionType string  `json:"extensionType"`
			FusionWebURL  string  `json:"fusionWebUrl"`
			CreatedOn     string  `json:"createdOn"`
			CreatedBy     apiUser `json:"createdBy"`
			ModifiedOn    string  `json:"lastModifiedOn"`
			ModifiedBy    apiUser `json:"lastModifiedBy"`
			TipVersion    struct {
				VersionNumber int `json:"versionNumber"`
			} `json:"tipVersion"`
		} `json:"item"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("item details decode: %w", err)
	}

	return &ItemDetails{
		ID:            raw.Item.ID,
		Name:          raw.Item.Name,
		Typename:      raw.Item.Typename,
		Size:          raw.Item.Size,
		MimeType:      raw.Item.MimeType,
		ExtensionType: raw.Item.ExtensionType,
		FusionWebURL:  raw.Item.FusionWebURL,
		CreatedOn:     parseTime(raw.Item.CreatedOn),
		CreatedBy:     raw.Item.CreatedBy.fullName(),
		ModifiedOn:    parseTime(raw.Item.ModifiedOn),
		ModifiedBy:    raw.Item.ModifiedBy.fullName(),
		VersionNumber: raw.Item.TipVersion.VersionNumber,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func navItemFromTypename(id, name, typename string) NavItem {
	kind := "unknown"
	isContainer := false
	switch typename {
	case "DesignItem":
		kind = "design"
	case "ConfiguredDesignItem":
		kind = "configured"
	case "DrawingItem":
		kind = "drawing"
	case "Folder":
		kind = "folder"
		isContainer = true
	case "BasicItem":
		kind = "basic"
	}
	return NavItem{ID: id, Name: name, Kind: kind, IsContainer: isContainer}
}

type apiUser struct {
	First string `json:"firstName"`
	Last  string `json:"lastName"`
}

func (u apiUser) fullName() string {
	name := u.First
	if u.Last != "" {
		if name != "" {
			name += " "
		}
		name += u.Last
	}
	return name
}

func parseSizeStr(s string) int64 {
	var size int64
	fmt.Sscanf(s, "%d", &size)
	return size
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t, _ = time.Parse("2006-01-02T15:04:05.000Z", s)
	}
	return t
}
