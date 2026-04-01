package fusiondata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const graphqlEndpoint = "https://developer.api.autodesk.com/mfg/graphql"

// region is the X-Ads-Region header value sent with every request.
var region string

// SetRegion configures the ADS region header (e.g. "EMEA", "AUS").
func SetRegion(r string) {
	if r == "US" {
		r = ""
	}
	region = r
}

// NavItem is a navigable node in the APS Manufacturing Data Model hierarchy.
type NavItem struct {
	ID          string
	Name        string
	Kind        string // "hub" | "project" | "folder" | "design" | "configured" | "drawing" | "unknown"
	AltID       string // alternative identifier (Data Management API ID)
	WebURL      string
	IsContainer bool
	Size        int64
	ModTime     time.Time
	MimeType    string
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// gqlQuery executes a GraphQL query against the APS Manufacturing Data API.
// It uses the provided token for authorization.
func gqlQuery(ctx context.Context, token, q string, vars map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(gqlRequest{Query: q, Variables: vars})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if region != "" {
		req.Header.Set("X-Ads-Region", region)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized — token may be expired (re-run rclone config)")
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var gr gqlResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, fmt.Errorf("parsing GraphQL response: %w", err)
	}
	if len(gr.Errors) > 0 {
		msgs := make([]string, len(gr.Errors))
		for i, e := range gr.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("GraphQL errors: %s", strings.Join(msgs, "; "))
	}
	if len(gr.Data) == 0 {
		return nil, fmt.Errorf("empty GraphQL response (HTTP %d)", resp.StatusCode)
	}
	return gr.Data, nil
}

// pageResult holds one page of paginated results.
type pageResult struct {
	cursor string
	data   json.RawMessage
}

// allPages calls the API repeatedly until no next-page cursor is returned.
func allPages(
	ctx context.Context,
	token string,
	queryFirst, queryNext string,
	baseVars map[string]any,
	extract func(json.RawMessage) (pageResult, error),
) ([]json.RawMessage, error) {
	var pages []json.RawMessage
	var cursor string
	first := true

	for {
		vars := make(map[string]any, len(baseVars)+1)
		for k, v := range baseVars {
			vars[k] = v
		}

		var q string
		if first {
			q = queryFirst
			first = false
		} else {
			q = queryNext
			vars["cursor"] = cursor
		}

		data, err := gqlQuery(ctx, token, q, vars)
		if err != nil {
			return nil, err
		}

		pr, err := extract(data)
		if err != nil {
			return nil, err
		}
		pages = append(pages, pr.data)
		cursor = pr.cursor
		if cursor == "" {
			break
		}
	}
	return pages, nil
}
