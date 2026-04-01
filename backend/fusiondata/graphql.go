package fusiondata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const graphqlEndpoint = "https://developer.api.autodesk.com/mfg/graphql"

const (
	maxRetries    = 5
	minRetryDelay = 2 * time.Second
	maxRetryDelay = 30 * time.Second
)

// apiThrottle combines a rate limiter ticker with a concurrency semaphore.
type apiThrottle struct {
	ticker *time.Ticker
	sem    chan struct{}
}

// throttle is the package-level throttle instance, initialized by initThrottle.
var throttle *apiThrottle

// initThrottle creates the package-level throttle with the given rate per second.
func initThrottle(ratePerSecond int) {
	if ratePerSecond <= 0 {
		ratePerSecond = 5
	}
	interval := time.Second / time.Duration(ratePerSecond)
	throttle = &apiThrottle{
		ticker: time.NewTicker(interval),
		sem:    make(chan struct{}, 10),
	}
}

// acquire waits for both a semaphore slot and a rate limiter tick.
func (t *apiThrottle) acquire(ctx context.Context) error {
	// Acquire semaphore first.
	select {
	case t.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Wait for rate limiter tick.
	select {
	case <-t.ticker.C:
	case <-ctx.Done():
		// Release semaphore on cancel.
		<-t.sem
		return ctx.Err()
	}
	return nil
}

// release returns the semaphore slot.
func (t *apiThrottle) release() {
	<-t.sem
}

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
// It uses the provided HTTP client (which should be an oauth2 client that
// auto-injects Bearer tokens). Includes rate limiting and automatic retry
// with exponential backoff on rate-limit errors.
func gqlQuery(ctx context.Context, client *http.Client, q string, vars map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(gqlRequest{Query: q, Variables: vars})
	if err != nil {
		return nil, err
	}

	var lastErr error
	delay := minRetryDelay

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
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

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlEndpoint, bytes.NewReader(body))
		if err != nil {
			if throttle != nil {
				throttle.release()
			}
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if region != "" {
			req.Header.Set("X-Ads-Region", region)
		}

		resp, err := client.Do(req)
		if throttle != nil {
			throttle.release()
		}
		if err != nil {
			lastErr = err
			continue
		}

		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		// Retry on 429 Too Many Requests.
		if resp.StatusCode == http.StatusTooManyRequests {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					delay = time.Duration(secs) * time.Second
				}
			}
			lastErr = fmt.Errorf("rate limited (HTTP 429)")
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("unauthorized — token may be expired (re-run rclone config)")
		}

		// Retry on 5xx server errors.
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("server error (HTTP %d)", resp.StatusCode)
			continue
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
			errMsg := strings.Join(msgs, "; ")
			// Retry on rate limit errors from downstream.
			if strings.Contains(errMsg, "Too many requests") || strings.Contains(errMsg, "quota") {
				lastErr = fmt.Errorf("GraphQL errors: %s", errMsg)
				continue
			}
			return nil, fmt.Errorf("GraphQL errors: %s", errMsg)
		}
		if len(gr.Data) == 0 {
			return nil, fmt.Errorf("empty GraphQL response (HTTP %d)", resp.StatusCode)
		}
		return gr.Data, nil
	}

	return nil, fmt.Errorf("giving up after %d retries: %w", maxRetries, lastErr)
}

// gqlQueryWithToken creates a temporary oauth2 HTTP client from a raw token
// string and executes a GraphQL query. This is used during config when no Fs
// struct (and therefore no persistent HTTP client) exists yet.
func gqlQueryWithToken(ctx context.Context, token, q string, vars map[string]any) (json.RawMessage, error) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	client := oauth2.NewClient(ctx, ts)
	return gqlQuery(ctx, client, q, vars)
}

// pageResult holds one page of paginated results.
type pageResult struct {
	cursor string
	data   json.RawMessage
}

// allPages calls the API repeatedly until no next-page cursor is returned.
func allPages(
	ctx context.Context,
	client *http.Client,
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

		data, err := gqlQuery(ctx, client, q, vars)
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

// allPagesWithToken is like allPages but uses a raw token string.
// Used during config when no Fs exists yet.
func allPagesWithToken(
	ctx context.Context,
	token string,
	queryFirst, queryNext string,
	baseVars map[string]any,
	extract func(json.RawMessage) (pageResult, error),
) ([]json.RawMessage, error) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	client := oauth2.NewClient(ctx, ts)
	return allPages(ctx, client, queryFirst, queryNext, baseVars, extract)
}
