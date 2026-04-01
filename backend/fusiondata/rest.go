package fusiondata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rclone/rclone/fs"
)

const (
	dmBaseURL  = "https://developer.api.autodesk.com"
	dmDataPath = "/data/v1"
	ossPath    = "/oss/v2"
)

// createStorage creates an OSS storage location for a file upload.
// Returns the storage object ID (URN).
func (f *Fs) createStorage(ctx context.Context, token, projectDM, folderDM, filename string) (string, error) {
	payload := map[string]any{
		"jsonapi": map[string]string{"version": "1.0"},
		"data": map[string]any{
			"type": "objects",
			"attributes": map[string]string{
				"name": filename,
			},
			"relationships": map[string]any{
				"target": map[string]any{
					"data": map[string]string{
						"type": "folders",
						"id":   folderDM,
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s%s/projects/%s/storage", dmBaseURL, dmDataPath, projectDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create storage failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	var result struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parsing storage response: %w", err)
	}

	return result.Data.ID, nil
}

// uploadToStorage uploads file content via the signed S3 upload flow.
// storageID is the URN returned by createStorage (format: "urn:adsk.objects:os.object:bucket/key").
func (f *Fs) uploadToStorage(ctx context.Context, token, storageID string, in io.Reader, size int64) error {
	bucketKey, objectKey, err := parseStorageURN(storageID)
	if err != nil {
		return err
	}

	// Step 1: Get signed S3 upload URLs.
	signedURL := fmt.Sprintf("%s%s/buckets/%s/objects/%s/signeds3upload", dmBaseURL, ossPath, bucketKey, objectKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get signed upload URL failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	var signedResult struct {
		URLs      []string `json:"urls"`
		UploadKey string   `json:"uploadKey"`
	}
	if err := json.Unmarshal(raw, &signedResult); err != nil {
		return fmt.Errorf("parsing signed upload response: %w", err)
	}

	if len(signedResult.URLs) == 0 {
		return fmt.Errorf("no upload URLs returned")
	}

	// Step 2: Upload content to the signed S3 URL.
	uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPut, signedResult.URLs[0], in)
	if err != nil {
		return err
	}
	uploadReq.Header.Set("Content-Type", "application/octet-stream")
	if size >= 0 {
		uploadReq.ContentLength = size
	}

	uploadResp, err := http.DefaultClient.Do(uploadReq)
	if err != nil {
		return err
	}
	defer uploadResp.Body.Close()

	if uploadResp.StatusCode != http.StatusOK && uploadResp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(uploadResp.Body)
		return fmt.Errorf("S3 upload failed (HTTP %d): %s", uploadResp.StatusCode, respBody)
	}

	// Step 3: Complete the upload.
	completePayload := map[string]string{
		"uploadKey": signedResult.UploadKey,
	}
	completeBody, err := json.Marshal(completePayload)
	if err != nil {
		return err
	}

	completeURL := fmt.Sprintf("%s%s/buckets/%s/objects/%s/signeds3upload", dmBaseURL, ossPath, bucketKey, objectKey)
	completeReq, err := http.NewRequestWithContext(ctx, http.MethodPost, completeURL, bytes.NewReader(completeBody))
	if err != nil {
		return err
	}
	completeReq.Header.Set("Authorization", "Bearer "+token)
	completeReq.Header.Set("Content-Type", "application/json")

	completeResp, err := http.DefaultClient.Do(completeReq)
	if err != nil {
		return err
	}
	defer completeResp.Body.Close()

	if completeResp.StatusCode != http.StatusOK && completeResp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(completeResp.Body)
		return fmt.Errorf("complete upload failed (HTTP %d): %s", completeResp.StatusCode, respBody)
	}

	return nil
}

// createFirstVersion creates the first version of an item (new file).
func (f *Fs) createFirstVersion(ctx context.Context, token, projectDM, folderDM, filename, storageID string) (string, error) {
	payload := map[string]any{
		"jsonapi": map[string]string{"version": "1.0"},
		"data": map[string]any{
			"type": "items",
			"attributes": map[string]any{
				"displayName": filename,
				"extension": map[string]any{
					"type":    "items:autodesk.core:File",
					"version": "1.0",
				},
			},
			"relationships": map[string]any{
				"tip": map[string]any{
					"data": map[string]any{
						"type": "versions",
						"id":   "1",
					},
				},
				"parent": map[string]any{
					"data": map[string]string{
						"type": "folders",
						"id":   folderDM,
					},
				},
			},
		},
		"included": []map[string]any{
			{
				"type": "versions",
				"id":   "1",
				"attributes": map[string]any{
					"name": filename,
					"extension": map[string]any{
						"type":    "versions:autodesk.core:File",
						"version": "1.0",
					},
				},
				"relationships": map[string]any{
					"storage": map[string]any{
						"data": map[string]string{
							"type": "objects",
							"id":   storageID,
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s%s/projects/%s/items", dmBaseURL, dmDataPath, projectDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create item failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	var result struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parsing create item response: %w", err)
	}

	return result.Data.ID, nil
}

// createNextVersion creates a subsequent version of an existing item.
func (f *Fs) createNextVersion(ctx context.Context, token, projectDM, itemDM, filename, storageID string) error {
	payload := map[string]any{
		"jsonapi": map[string]string{"version": "1.0"},
		"data": map[string]any{
			"type": "versions",
			"attributes": map[string]any{
				"name": filename,
				"extension": map[string]any{
					"type":    "versions:autodesk.core:File",
					"version": "1.0",
				},
			},
			"relationships": map[string]any{
				"item": map[string]any{
					"data": map[string]string{
						"type": "items",
						"id":   itemDM,
					},
				},
				"storage": map[string]any{
					"data": map[string]string{
						"type": "objects",
						"id":   storageID,
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s%s/projects/%s/versions", dmBaseURL, dmDataPath, projectDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create version failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	return nil
}

// getTopFolders returns the top-level folders for a project via the DM REST API.
// Each returned entry has its DM API folder ID.
func (f *Fs) getTopFolders(ctx context.Context, token, hubDM, projectDM string) ([]dmEntry, error) {
	url := fmt.Sprintf("%s/project/v1/hubs/%s/projects/%s/topFolders", dmBaseURL, hubDM, projectDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get top folders failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	var result struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}

	folders := make([]dmEntry, len(result.Data))
	for i, d := range result.Data {
		folders[i] = dmEntry{ID: d.ID, Name: d.Attributes.Name}
	}
	return folders, nil
}

// getFolderContents returns all entries (folders and items) inside a folder via the DM REST API.
func (f *Fs) getFolderContents(ctx context.Context, token, projectDM, folderDM string) ([]dmEntry, error) {
	reqURL := fmt.Sprintf("%s%s/projects/%s/folders/%s/contents", dmBaseURL, dmDataPath, projectDM, folderDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get folder contents failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	var result struct {
		Data []struct {
			Type       string `json:"type"`
			ID         string `json:"id"`
			Attributes struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}

	var entries []dmEntry
	for _, d := range result.Data {
		name := d.Attributes.DisplayName
		if name == "" {
			name = d.Attributes.Name
		}
		entries = append(entries, dmEntry{
			Type: d.Type, // "folders" or "items"
			ID:   d.ID,
			Name: name,
		})
	}
	return entries, nil
}

type dmEntry struct {
	Type string // "folders" or "items"
	ID   string
	Name string
}

// resolveItemDM finds the DM API item ID for a file by name within a folder.
func (f *Fs) resolveItemDM(ctx context.Context, token, projectDM, folderDM, itemName string) (string, error) {
	entries, err := f.getFolderContents(ctx, token, projectDM, folderDM)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Type == "items" && e.Name == itemName {
			return e.ID, nil
		}
	}
	return "", fmt.Errorf("item %q not found in folder via DM API", itemName)
}

// resolveFolderDMPath walks the folder path via the DM REST API to get the DM folder ID.
// folderNames is the list of folder names from the project root down.
//
// The DM API has an invisible root folder layer (e.g. "Project Files") between
// the project and the user-visible folders. The GraphQL API skips this layer,
// so folderNames[0] is a user-visible folder that lives INSIDE one of the top
// folders, not a top folder itself.
func (f *Fs) resolveFolderDMPath(ctx context.Context, token, hubDM, projectDM string, folderNames []string) (string, error) {
	if len(folderNames) == 0 {
		return "", nil
	}

	// Get top-level (root) folders — these are invisible containers like "Project Files".
	topFolders, err := f.getTopFolders(ctx, token, hubDM, projectDM)
	if err != nil {
		return "", fmt.Errorf("listing top folders: %w", err)
	}

	// First check if folderNames[0] IS a top folder (some projects expose them directly).
	var currentDM string
	for _, tf := range topFolders {
		if tf.Name == folderNames[0] {
			currentDM = tf.ID
			break
		}
	}

	// If not found at top level, search inside each top folder for the first folder name.
	if currentDM == "" {
		for _, tf := range topFolders {
			contents, err := f.getFolderContents(ctx, token, projectDM, tf.ID)
			if err != nil {
				continue
			}
			for _, entry := range contents {
				if entry.Type == "folders" && entry.Name == folderNames[0] {
					currentDM = entry.ID
					break
				}
			}
			if currentDM != "" {
				break
			}
		}
	}

	if currentDM == "" {
		return "", fmt.Errorf("folder %q not found via DM API", folderNames[0])
	}

	// Walk deeper folders.
	for i := 1; i < len(folderNames); i++ {
		subFolders, err := f.getFolderContents(ctx, token, projectDM, currentDM)
		if err != nil {
			return "", fmt.Errorf("listing subfolder contents: %w", err)
		}
		found := false
		for _, sf := range subFolders {
			if sf.Type == "folders" && sf.Name == folderNames[i] {
				currentDM = sf.ID
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("folder %q not found in %s via DM API", folderNames[i], strings.Join(folderNames[:i], "/"))
		}
	}

	return currentDM, nil
}

// getDownloadURLWithProject gets a signed download URL for an item within a project.
// Uses the tip version's storage URN to get a signed S3 download URL.
func (f *Fs) getDownloadURLWithProject(ctx context.Context, token, projectDM, itemDM string) (string, error) {
	// Step 1: Get the tip version to find the storage URN.
	tipURL := fmt.Sprintf("%s%s/projects/%s/items/%s/tip", dmBaseURL, dmDataPath, projectDM, itemDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tipURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get tip version failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	// Parse the storage ID from the version relationships.
	var tipResult struct {
		Data struct {
			Relationships struct {
				Storage struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"storage"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &tipResult); err != nil {
		return "", fmt.Errorf("parsing tip version: %w", err)
	}

	storageID := tipResult.Data.Relationships.Storage.Data.ID
	if storageID == "" {
		return "", fmt.Errorf("no storage ID in tip version response")
	}

	// Step 2: Parse the storage URN to get bucket and object key.
	bucketKey, objectKey, err := parseStorageURN(storageID)
	if err != nil {
		return "", fmt.Errorf("parsing storage URN: %w", err)
	}

	// Step 3: Get signed S3 download URL.
	signedURL := fmt.Sprintf("%s%s/buckets/%s/objects/%s/signeds3download", dmBaseURL, ossPath, bucketKey, objectKey)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get signed download URL failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	var signedResult struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &signedResult); err != nil {
		return "", fmt.Errorf("parsing signed download URL: %w", err)
	}

	if signedResult.URL == "" {
		return "", fmt.Errorf("empty signed download URL in response")
	}

	return signedResult.URL, nil
}

// createFolder creates a new folder under the given parent.
func (f *Fs) createFolder(ctx context.Context, token, projectDM, parentFolderDM, name string) (string, error) {
	payload := map[string]any{
		"jsonapi": map[string]string{"version": "1.0"},
		"data": map[string]any{
			"type": "folders",
			"attributes": map[string]any{
				"name": name,
				"extension": map[string]any{
					"type":    "folders:autodesk.core:Folder",
					"version": "1.0",
				},
			},
			"relationships": map[string]any{
				"parent": map[string]any{
					"data": map[string]string{
						"type": "folders",
						"id":   parentFolderDM,
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s%s/projects/%s/folders", dmBaseURL, dmDataPath, projectDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create folder failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	var result struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parsing create folder response: %w", err)
	}

	return result.Data.ID, nil
}

// parseStorageURN parses a storage URN into bucket key and object key.
// Format: urn:adsk.objects:os.object:bucketKey/objectKey
func parseStorageURN(urn string) (bucketKey, objectKey string, err error) {
	const prefix = "urn:adsk.objects:os.object:"
	if !strings.HasPrefix(urn, prefix) {
		return "", "", fmt.Errorf("unexpected storage URN format: %s", urn)
	}
	rest := urn[len(prefix):]
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("cannot parse bucket/object from URN: %s", urn)
	}
	return parts[0], parts[1], nil
}

// parseStorageHref parses a storage API href into bucket key and object key.
// Format: https://developer.api.autodesk.com/oss/v2/buckets/BUCKET/objects/KEY
func parseStorageHref(href string) (bucketKey, objectKey string, err error) {
	// Find "/buckets/" and "/objects/" in the URL.
	bucketsIdx := strings.Index(href, "/buckets/")
	if bucketsIdx == -1 {
		return "", "", fmt.Errorf("no /buckets/ in storage href: %s", href)
	}
	rest := href[bucketsIdx+len("/buckets/"):]
	objectsIdx := strings.Index(rest, "/objects/")
	if objectsIdx == -1 {
		return "", "", fmt.Errorf("no /objects/ in storage href: %s", href)
	}
	bucketKey = rest[:objectsIdx]
	objectKey = rest[objectsIdx+len("/objects/"):]
	return bucketKey, objectKey, nil
}

// Move renames/moves a file from srcRemote to dstRemote.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	// APS does not have a simple move/rename API for items.
	// This would require: rename via PATCH, or copy+delete.
	return nil, fs.ErrorCantMove
}

// DirMove moves a directory from srcRemote to dstRemote.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	return fs.ErrorCantDirMove
}
