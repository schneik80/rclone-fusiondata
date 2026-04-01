package fusiondata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
)

const (
	dmBaseURL  = "https://developer.api.autodesk.com"
	dmDataPath = "/data/v1"
	ossPath    = "/oss/v2"

	defaultChunkSize = 100 * 1024 * 1024 // 100 MB
)

// doAPIRequest executes an HTTP request through the oauth2 client with
// throttling and retry logic for 429/5xx responses.
func (f *Fs) doAPIRequest(ctx context.Context, req *http.Request) (*http.Response, error) {
	fs.Debugf(f, "doAPIRequest: %s %s", req.Method, req.URL.String())
	var lastErr error
	delay := minRetryDelay

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			fs.Debugf(f, "doAPIRequest: retry attempt %d/%d delay=%v for %s %s", attempt, maxRetries, delay, req.Method, req.URL.String())
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			delay *= 2
			if delay > maxRetryDelay {
				delay = maxRetryDelay
			}
		}

		// Throttle: acquire semaphore + rate limit.
		if throttle != nil {
			if err := throttle.acquire(ctx); err != nil {
				return nil, err
			}
		}

		// Clone the request body for retries if it has one.
		var bodyBytes []byte
		if req.Body != nil && attempt == 0 {
			var err error
			bodyBytes, err = io.ReadAll(req.Body)
			if err != nil {
				if throttle != nil {
					throttle.release()
				}
				return nil, err
			}
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			// Store for retries.
			req = req.WithContext(ctx)
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		} else if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		resp, err := f.srv.Do(req)
		if throttle != nil {
			throttle.release()
		}
		if err != nil {
			lastErr = err
			continue
		}

		// Handle 401 Unauthorized.
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return nil, fs.ErrorPermissionDenied
		}

		// Handle 403 Forbidden.
		if resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			return nil, fmt.Errorf("access denied (HTTP 403): %w", fs.ErrorPermissionDenied)
		}

		// Handle 404 Not Found.
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return nil, fs.ErrorObjectNotFound
		}

		// Handle 429 Too Many Requests — retry.
		if resp.StatusCode == http.StatusTooManyRequests {
			fs.Infof(f, "doAPIRequest: rate limited (429) for %s %s, will retry", req.Method, req.URL.String())
			resp.Body.Close()
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, parseErr := strconv.Atoi(ra); parseErr == nil {
					delay = time.Duration(secs) * time.Second
				}
			}
			lastErr = fmt.Errorf("rate limited (HTTP 429)")
			continue
		}

		// Handle 5xx Server Errors — retry.
		if resp.StatusCode >= 500 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("server error (HTTP %d): %s", resp.StatusCode, body)
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("giving up after %d retries: %w", maxRetries, lastErr)
}

// createStorage creates an OSS storage location for a file upload.
// Returns the storage object ID (URN).
func (f *Fs) createStorage(ctx context.Context, projectDM, folderDM, filename string) (string, error) {
	fs.Debugf(f, "createStorage: project=%q folder=%q filename=%q", projectDM, folderDM, filename)
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

	resp, err := f.doAPIRequest(ctx, req)
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
// For files larger than the chunk size, uses multipart upload.
func (f *Fs) uploadToStorage(ctx context.Context, storageID string, in io.Reader, size int64) error {
	bucketKey, objectKey, err := parseStorageURN(storageID)
	if err != nil {
		return err
	}

	chunkSize := int64(defaultChunkSize)
	if f.opt.UploadChunkSize > 0 {
		chunkSize = int64(f.opt.UploadChunkSize)
	}

	if size > 0 && size > chunkSize {
		fs.Debugf(f, "uploadToStorage: URN=%q size=%d using multipart (chunkSize=%d)", storageID, size, chunkSize)
		return f.uploadMultipart(ctx, bucketKey, objectKey, in, size, chunkSize)
	}
	fs.Debugf(f, "uploadToStorage: URN=%q size=%d using single part", storageID, size)
	return f.uploadSinglePart(ctx, bucketKey, objectKey, in, size)
}

// uploadSinglePart uploads file content as a single part via signed S3 URL.
func (f *Fs) uploadSinglePart(ctx context.Context, bucketKey, objectKey string, in io.Reader, size int64) error {
	// Step 1: Get signed S3 upload URLs.
	signedURL := fmt.Sprintf("%s%s/buckets/%s/objects/%s/signeds3upload", dmBaseURL, ossPath, bucketKey, objectKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return err
	}

	resp, err := f.doAPIRequest(ctx, req)
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

	// Step 2: Upload content to the signed S3 URL (no auth needed — pre-signed).
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
	completeReq.Header.Set("Content-Type", "application/json")

	completeResp, err := f.doAPIRequest(ctx, completeReq)
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

// uploadMultipart uploads file content in multiple parts via signed S3 URLs.
func (f *Fs) uploadMultipart(ctx context.Context, bucketKey, objectKey string, in io.Reader, size, chunkSize int64) error {
	numParts := int(math.Ceil(float64(size) / float64(chunkSize)))
	if numParts < 2 {
		numParts = 2
	}

	// Step 1: Get signed S3 upload URLs for N parts.
	signedURL := fmt.Sprintf("%s%s/buckets/%s/objects/%s/signeds3upload?parts=%d",
		dmBaseURL, ossPath, bucketKey, objectKey, numParts)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return err
	}

	resp, err := f.doAPIRequest(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get signed multipart upload URLs failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	var signedResult struct {
		URLs      []string `json:"urls"`
		UploadKey string   `json:"uploadKey"`
	}
	if err := json.Unmarshal(raw, &signedResult); err != nil {
		return fmt.Errorf("parsing signed multipart upload response: %w", err)
	}

	if len(signedResult.URLs) < numParts {
		return fmt.Errorf("expected %d upload URLs, got %d", numParts, len(signedResult.URLs))
	}

	// Step 2: Upload each part and collect ETags.
	etags := make([]string, numParts)
	remaining := size

	for i := 0; i < numParts; i++ {
		partSize := chunkSize
		if remaining < partSize {
			partSize = remaining
		}
		remaining -= partSize

		fs.Infof(f, "uploadMultipart: uploading part %d/%d size=%d", i+1, numParts, partSize)
		partReader := io.LimitReader(in, partSize)

		uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPut, signedResult.URLs[i], partReader)
		if err != nil {
			return fmt.Errorf("creating part %d upload request: %w", i+1, err)
		}
		uploadReq.Header.Set("Content-Type", "application/octet-stream")
		uploadReq.ContentLength = partSize

		// S3 signed URLs don't need auth headers.
		uploadResp, err := http.DefaultClient.Do(uploadReq)
		if err != nil {
			return fmt.Errorf("uploading part %d: %w", i+1, err)
		}

		if uploadResp.StatusCode != http.StatusOK && uploadResp.StatusCode != http.StatusCreated {
			respBody, _ := io.ReadAll(uploadResp.Body)
			uploadResp.Body.Close()
			return fmt.Errorf("S3 multipart upload part %d failed (HTTP %d): %s", i+1, uploadResp.StatusCode, respBody)
		}

		etag := uploadResp.Header.Get("ETag")
		uploadResp.Body.Close()
		etags[i] = etag
	}

	// Step 3: Complete the multipart upload with ETags.
	completePayload := map[string]any{
		"uploadKey": signedResult.UploadKey,
		"eTags":     etags,
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
	completeReq.Header.Set("Content-Type", "application/json")

	completeResp, err := f.doAPIRequest(ctx, completeReq)
	if err != nil {
		return err
	}
	defer completeResp.Body.Close()

	if completeResp.StatusCode != http.StatusOK && completeResp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(completeResp.Body)
		return fmt.Errorf("complete multipart upload failed (HTTP %d): %s", completeResp.StatusCode, respBody)
	}

	return nil
}

// createFirstVersion creates the first version of an item (new file).
func (f *Fs) createFirstVersion(ctx context.Context, projectDM, folderDM, filename, storageID string) (string, error) {
	fs.Debugf(f, "createFirstVersion: project=%q folder=%q filename=%q", projectDM, folderDM, filename)
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

	resp, err := f.doAPIRequest(ctx, req)
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
func (f *Fs) createNextVersion(ctx context.Context, projectDM, itemDM, filename, storageID string) error {
	fs.Debugf(f, "createNextVersion: project=%q item=%q filename=%q", projectDM, itemDM, filename)
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

	resp, err := f.doAPIRequest(ctx, req)
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
func (f *Fs) getTopFolders(ctx context.Context, hubDM, projectDM string) ([]dmEntry, error) {
	fs.Debugf(f, "getTopFolders: hub=%q project=%q", hubDM, projectDM)
	url := fmt.Sprintf("%s/project/v1/hubs/%s/projects/%s/topFolders", dmBaseURL, hubDM, projectDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := f.doAPIRequest(ctx, req)
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
	fs.Debugf(f, "getTopFolders: project=%q found %d folders", projectDM, len(folders))
	return folders, nil
}

// getFolderContents returns all entries (folders and items) inside a folder via the DM REST API.
func (f *Fs) getFolderContents(ctx context.Context, projectDM, folderDM string) ([]dmEntry, error) {
	fs.Debugf(f, "getFolderContents: project=%q folder=%q", projectDM, folderDM)
	reqURL := fmt.Sprintf("%s%s/projects/%s/folders/%s/contents", dmBaseURL, dmDataPath, projectDM, folderDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := f.doAPIRequest(ctx, req)
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
	fs.Debugf(f, "getFolderContents: folder=%q found %d entries", folderDM, len(entries))
	return entries, nil
}

type dmEntry struct {
	Type string // "folders" or "items"
	ID   string
	Name string
}

// resolveItemDM finds the DM API item ID for a file by name within a folder.
func (f *Fs) resolveItemDM(ctx context.Context, projectDM, folderDM, itemName string) (string, error) {
	entries, err := f.getFolderContents(ctx, projectDM, folderDM)
	if err != nil {
		return "", err
	}
	// Try exact match first, then match with fusion extension stripped.
	// The display name may have a fusion extension (e.g. "Part.fusiondesign")
	// but the DM API returns the server-side name (e.g. "Part").
	stripped := stripFusionExtension(itemName)
	for _, e := range entries {
		if e.Type == "items" && (e.Name == itemName || e.Name == stripped) {
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
func (f *Fs) resolveFolderDMPath(ctx context.Context, hubDM, projectDM string, folderNames []string) (string, error) {
	fs.Debugf(f, "resolveFolderDMPath: walking path %v in project=%q", folderNames, projectDM)
	if len(folderNames) == 0 {
		return "", nil
	}

	// Get top-level (root) folders — these are invisible containers like "Project Files".
	topFolders, err := f.getTopFolders(ctx, hubDM, projectDM)
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
			contents, err := f.getFolderContents(ctx, projectDM, tf.ID)
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
	fs.Debugf(f, "resolveFolderDMPath: resolved %q -> %q", folderNames[0], currentDM)

	// Walk deeper folders.
	for i := 1; i < len(folderNames); i++ {
		subFolders, err := f.getFolderContents(ctx, projectDM, currentDM)
		if err != nil {
			return "", fmt.Errorf("listing subfolder contents: %w", err)
		}
		found := false
		for _, sf := range subFolders {
			if sf.Type == "folders" && sf.Name == folderNames[i] {
				currentDM = sf.ID
				found = true
				fs.Debugf(f, "resolveFolderDMPath: resolved %q -> %q", folderNames[i], currentDM)
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
func (f *Fs) getDownloadURLWithProject(ctx context.Context, projectDM, itemDM string) (string, error) {
	fs.Debugf(f, "getDownloadURLWithProject: project=%q item=%q", projectDM, itemDM)
	// Step 1: Get the tip version to find the storage URN.
	tipURL := fmt.Sprintf("%s%s/projects/%s/items/%s/tip", dmBaseURL, dmDataPath, projectDM, itemDM)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tipURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := f.doAPIRequest(ctx, req)
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

	resp, err = f.doAPIRequest(ctx, req)
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
func (f *Fs) createFolder(ctx context.Context, projectDM, parentFolderDM, name string) (string, error) {
	fs.Debugf(f, "createFolder: project=%q parent=%q name=%q", projectDM, parentFolderDM, name)
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

	resp, err := f.doAPIRequest(ctx, req)
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
