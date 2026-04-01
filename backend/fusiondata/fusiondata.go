// Package fusiondata implements an rclone backend for Autodesk Fusion Data (APS).
//
// The backend uses the APS Manufacturing Data Model GraphQL API for reading
// (listing hubs, projects, folders, items) and the Data Management REST API
// for write operations (upload, download, create folder, delete).
//
// Users authenticate via 3-legged OAuth2 with PKCE. During rclone config the
// user selects one hub as the root; projects inside that hub appear as
// top-level directories.
package fusiondata

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/oauthutil"
	"golang.org/x/oauth2"
)

const (
	authEndpoint  = "https://developer.api.autodesk.com/authentication/v2/authorize"
	tokenEndpoint = "https://developer.api.autodesk.com/authentication/v2/token"
	callbackURL   = "http://localhost:7879/callback"
	callbackPort  = 7879
)

var oauthConfig = &oauth2.Config{
	Endpoint: oauth2.Endpoint{
		AuthURL:   authEndpoint,
		TokenURL:  tokenEndpoint,
		AuthStyle: oauth2.AuthStyleInParams,
	},
	Scopes:      []string{"data:read", "data:write", "data:create", "user-profile:read"},
	RedirectURL: callbackURL,
}

// Options defines the configuration for the Fusion Data backend.
type Options struct {
	ClientID        string        `config:"client_id"`
	ClientSecret    string        `config:"client_secret"`
	HubID           string        `config:"hub_id"`
	HubName         string        `config:"hub_name"`
	Region          string        `config:"region"`
	CacheTTL        fs.Duration   `config:"cache_ttl"`
	RateLimit       int           `config:"rate_limit"`
	UploadChunkSize fs.SizeSuffix `config:"upload_chunk_size"`
}

func init() {
	fsi := &fs.RegInfo{
		Name:        "fusiondata",
		Description: "Autodesk Fusion Data (APS)",
		NewFs:       NewFs,
		Config: func(ctx context.Context, name string, m configmap.Mapper, config fs.ConfigIn) (*fs.ConfigOut, error) {
			return configHandler(ctx, name, m, config)
		},
		Options: fs.Options{
			{
				Name:     "client_id",
				Help:     "APS OAuth2 Client ID.\n\nRegister an app at https://aps.autodesk.com to obtain one.",
				Required: true,
			},
			{
				Name:       "client_secret",
				Help:       "APS OAuth2 Client Secret.\n\nLeave blank for public PKCE clients.",
				IsPassword: true,
			},
			{
				Name:     "hub_id",
				Help:     "GraphQL Hub ID (set during config).",
				Advanced: true,
			},
			{
				Name:     "hub_name",
				Help:     "Hub display name (set during config).",
				Advanced: true,
			},
			{
				Name:     "region",
				Help:     "APS region.",
				Default:  "US",
				Advanced: true,
				Examples: []fs.OptionExample{
					{Value: "US", Help: "United States (default)"},
					{Value: "EMEA", Help: "Europe, Middle East, Africa"},
					{Value: "AUS", Help: "Australia"},
				},
			},
			{
				Name:     "cache_ttl",
				Help:     "TTL for internal path cache entries.",
				Default:  fs.Duration(5 * time.Minute),
				Advanced: true,
			},
			{
				Name:     "rate_limit",
				Help:     "Maximum API requests per second.",
				Default:  5,
				Advanced: true,
			},
			{
				Name:     "upload_chunk_size",
				Help:     "Chunk size for multipart uploads.",
				Default:  fs.SizeSuffix(100 * 1024 * 1024),
				Advanced: true,
			},
		},
	}
	fs.Register(fsi)
}

// configHandler implements the multi-step configuration state machine:
//
//	"" (initial) -> perform OAuth2 PKCE login
//	"oauth_done" -> list hubs and ask user to choose
//	"hub_chosen" -> save hub_id and finish
func configHandler(ctx context.Context, name string, m configmap.Mapper, config fs.ConfigIn) (*fs.ConfigOut, error) {
	fs.Debugf(nil, "Config handler: state=%q", config.State)
	switch config.State {
	case "":
		// Step 1: Perform OAuth2 PKCE flow.
		clientID, _ := m.Get("client_id")
		clientSecret, _ := m.Get("client_secret")
		if clientID == "" {
			return nil, errors.New("client_id is required")
		}

		token, err := doOAuthLogin(ctx, clientID, clientSecret)
		if err != nil {
			return nil, fmt.Errorf("OAuth login failed: %w", err)
		}

		// Save token in rclone's oauthutil format.
		tokenJSON, err := json.Marshal(token)
		if err != nil {
			return nil, err
		}
		m.Set("token", string(tokenJSON))

		fs.Debugf(nil, "Config handler: OAuth login complete, transitioning to oauth_done")
		return fs.ConfigGoto("oauth_done")

	case "oauth_done":
		// Step 2: List hubs and let user choose.
		// Uses token-based query since there is no Fs struct yet.
		clientID, _ := m.Get("client_id")
		region, _ := m.Get("region")
		tokenJSON, _ := m.Get("token")
		if tokenJSON == "" {
			return nil, errors.New("no token found — please re-run config")
		}

		var token oauth2.Token
		if err := json.Unmarshal([]byte(tokenJSON), &token); err != nil {
			return nil, fmt.Errorf("parsing saved token: %w", err)
		}

		_ = clientID // available if needed
		SetRegion(region)

		hubs, err := GetHubsWithToken(ctx, token.AccessToken)
		if err != nil {
			return nil, fmt.Errorf("listing hubs: %w", err)
		}
		if len(hubs) == 0 {
			return nil, errors.New("no hubs found — check your Autodesk account has Fusion Team access")
		}

		choices := make([]string, len(hubs))
		helps := make([]string, len(hubs))
		for i, h := range hubs {
			choices[i] = h.ID
			helps[i] = h.Name
		}

		return fs.ConfigChooseExclusive("hub_chosen", "hub_id", "Select a hub as the root of this remote", len(hubs), func(i int) (string, string) {
			return choices[i], helps[i]
		})

	case "hub_chosen":
		// Step 3: Save hub selection.
		fs.Debugf(nil, "Config handler: hub chosen, saving selection")
		hubID := config.Result
		if hubID == "" {
			return nil, errors.New("no hub selected")
		}
		m.Set("hub_id", hubID)

		// Also save the hub name for display.
		tokenJSON, _ := m.Get("token")
		region, _ := m.Get("region")
		var token oauth2.Token
		if err := json.Unmarshal([]byte(tokenJSON), &token); err == nil {
			SetRegion(region)
			if hubs, err := GetHubsWithToken(ctx, token.AccessToken); err == nil {
				for _, h := range hubs {
					if h.ID == hubID {
						m.Set("hub_name", h.Name)
						break
					}
				}
			}
		}

		return nil, nil // config complete
	}

	return nil, fmt.Errorf("unknown config state: %q", config.State)
}

// persistingTokenSource wraps an oauth2.TokenSource and persists refreshed
// tokens back to the rclone config via configmap.Mapper.
type persistingTokenSource struct {
	base      oauth2.TokenSource
	m         configmap.Mapper
	lastToken string // last known access token to detect changes
	mu        sync.Mutex
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.AccessToken != p.lastToken {
		p.lastToken = tok.AccessToken
		tokenJSON, _ := json.Marshal(tok)
		p.m.Set("token", string(tokenJSON))
	}
	return tok, nil
}

// doOAuthLogin performs the full PKCE OAuth2 flow: opens browser, waits for
// callback, exchanges code for tokens.
func doOAuthLogin(ctx context.Context, clientID, clientSecret string) (*oauth2.Token, error) {
	verifier, err := newPKCEVerifier()
	if err != nil {
		return nil, err
	}
	challenge := pkceChallenge(verifier)

	authURL := buildAuthURL(clientID, challenge)
	if err := oauthutil.OpenURL(authURL); err != nil {
		fs.Logf(nil, "Open this URL to authenticate:\n\n  %s\n", authURL)
	}

	code, err := waitForCallback(ctx)
	if err != nil {
		return nil, err
	}

	return exchangeCode(ctx, clientID, clientSecret, code, verifier)
}

func newPKCEVerifier() (string, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func buildAuthURL(clientID, challenge string) string {
	p := url.Values{}
	p.Set("client_id", clientID)
	p.Set("response_type", "code")
	p.Set("redirect_uri", callbackURL)
	p.Set("scope", strings.Join(oauthConfig.Scopes, " "))
	p.Set("code_challenge", challenge)
	p.Set("code_challenge_method", "S256")
	return authEndpoint + "?" + p.Encode()
}

func exchangeCode(ctx context.Context, clientID, clientSecret, code, verifier string) (*oauth2.Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", callbackURL)
	form.Set("code_verifier", verifier)

	if clientSecret != "" {
		// Confidential client: Basic Auth.
	} else {
		form.Set("client_id", clientID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if clientSecret != "" {
		req.SetBasicAuth(clientID, clientSecret)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("parsing token response (HTTP %d): %w", resp.StatusCode, err)
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("token error %s: %s", tr.Error, tr.ErrorDesc)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request failed (HTTP %d): %s", resp.StatusCode, raw)
	}

	return &oauth2.Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// waitForCallback starts a local HTTP server and waits for the OAuth redirect.
func waitForCallback(ctx context.Context) (string, error) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if e := r.URL.Query().Get("error"); e != "" {
			desc := r.URL.Query().Get("error_description")
			fmt.Fprintf(w, "<html><body><h2>Authentication failed</h2><p>%s: %s</p><p>You can close this window.</p></body></html>", e, desc)
			errCh <- fmt.Errorf("oauth error: %s — %s", e, desc)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			fmt.Fprintf(w, "<html><body><h2>Authentication failed</h2><p>No code received.</p></body></html>")
			errCh <- fmt.Errorf("no authorization code in callback")
			return
		}
		fmt.Fprintf(w, "<html><body><h2>Authentication successful!</h2><p>Return to your terminal.</p></body></html>")
		codeCh <- code
	})

	ln, err := listenOnPort(callbackPort)
	if err != nil {
		return "", err
	}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			select {
			case errCh <- err:
			default:
			}
		}
	}()
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	select {
	case code := <-codeCh:
		return code, nil
	case err := <-errCh:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func listenOnPort(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", port))
}

// Fs represents the Fusion Data remote filesystem rooted at a single hub.
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	srv      *http.Client
	ts       oauth2.TokenSource
	m        configmap.Mapper
	hubID    string
	hubDM    string // Data Management API hub ID
	region   string
	cache    *pathCache
}

// NewFs creates a new Fusion Data Fs from the config.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}

	if opt.HubID == "" {
		return nil, errors.New("hub_id not set — run rclone config to select a hub")
	}

	// Initialize the API throttle from configured rate limit.
	rateLimit := opt.RateLimit
	if rateLimit <= 0 {
		rateLimit = 5
	}
	initThrottle(rateLimit)

	// Set up OAuth2 token source for automatic refresh.
	oauthCfg := *oauthConfig
	oauthCfg.ClientID = opt.ClientID
	oauthCfg.ClientSecret = opt.ClientSecret

	tokenJSON, _ := m.Get("token")
	if tokenJSON == "" {
		return nil, errors.New("no OAuth token — run rclone config to authenticate")
	}
	var token oauth2.Token
	if err := json.Unmarshal([]byte(tokenJSON), &token); err != nil {
		return nil, fmt.Errorf("invalid OAuth token: %w", err)
	}

	baseTS := oauthCfg.TokenSource(ctx, &token)
	ts := &persistingTokenSource{
		base:      baseTS,
		m:         m,
		lastToken: token.AccessToken,
	}
	client := oauth2.NewClient(ctx, ts)

	// Determine cache TTL from options.
	cacheTTL := time.Duration(opt.CacheTTL)
	if cacheTTL <= 0 {
		cacheTTL = 5 * time.Minute
	}

	f := &Fs{
		name:   name,
		root:   root,
		opt:    *opt,
		srv:    client,
		ts:     ts,
		m:      m,
		hubID:  opt.HubID,
		region: opt.Region,
		cache:  newPathCache(cacheTTL),
	}

	fs.Infof(f, "NewFs: hub=%q region=%q cacheTTL=%v root=%q", opt.HubName, opt.Region, cacheTTL, root)

	SetRegion(opt.Region)

	// Resolve hub DM ID for REST API write operations using the oauth2 client.
	hubs, err := GetHubs(ctx, f.srv)
	if err == nil {
		for _, h := range hubs {
			if h.ID == opt.HubID {
				f.hubDM = h.AltID
				fs.Debugf(f, "NewFs: resolved hub DM ID=%q", f.hubDM)
				break
			}
		}
	}

	f.features = (&fs.Features{
		DuplicateFiles:          true,
		ReadMimeType:            true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	// If root is a file, return fs.ErrorIsFile per rclone convention.
	if root != "" {
		_, err := f.NewObject(ctx, "")
		if err == nil {
			// Root is a file — trim the last path component.
			newRoot, leaf := path.Split(root)
			f.root = strings.TrimSuffix(newRoot, "/")
			_ = leaf
			return f, fs.ErrorIsFile
		}
	}

	return f, nil
}

// Name returns the configured name of this remote.
func (f *Fs) Name() string { return f.name }

// Root returns the root path.
func (f *Fs) Root() string { return f.root }

// String returns a description of the Fs.
func (f *Fs) String() string {
	if f.opt.HubName != "" {
		return fmt.Sprintf("Fusion Data [%s]", f.opt.HubName)
	}
	return fmt.Sprintf("Fusion Data [%s]", f.hubID)
}

// Precision returns the supported time precision.
func (f *Fs) Precision() time.Duration { return time.Second }

// Hashes returns the supported hash types (none for Fusion Data).
func (f *Fs) Hashes() hash.Set { return hash.Set(hash.None) }

// Features returns the optional features of this Fs.
func (f *Fs) Features() *fs.Features { return f.features }

// getToken returns a current access token, refreshing if needed.
func (f *Fs) getToken() (string, error) {
	tok, err := f.ts.Token()
	if err != nil {
		return "", fmt.Errorf("refreshing token: %w", err)
	}
	return tok.AccessToken, nil
}

// List lists the objects and directories in dir.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	fullDir := path.Join(f.root, dir)
	fs.Infof(f, "List: dir=%q fullDir=%q", dir, fullDir)

	resolved, err := f.resolvePath(ctx, fullDir)
	if err != nil {
		fs.Debugf(f, "List: dir=%q not found: %v", dir, err)
		return nil, fs.ErrorDirNotFound
	}

	var entries fs.DirEntries

	switch resolved.kind {
	case "hub":
		// List projects using the oauth2 HTTP client.
		projects, err := GetProjects(ctx, f.srv, f.hubID)
		if err != nil {
			return nil, err
		}
		for _, p := range projects {
			remote := trimRoot(f.root, p.Name)
			d := fs.NewDir(remote, time.Time{})
			entries = append(entries, d)
		}
		// Replace all children in cache atomically.
		f.cache.replaceChildren(f.hubID, projects)

	case "project":
		// List top-level folders in the project.
		folders, err := GetFolders(ctx, f.srv, resolved.id)
		if err != nil {
			return nil, err
		}
		for _, fld := range folders {
			remote := trimRoot(f.root, path.Join(dir, fld.Name))
			d := fs.NewDir(remote, time.Time{})
			entries = append(entries, d)
		}
		// Also list items at project root.
		items, err := GetProjectItems(ctx, f.srv, resolved.id)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.IsContainer {
				remote := trimRoot(f.root, path.Join(dir, it.Name))
				d := fs.NewDir(remote, time.Time{})
				entries = append(entries, d)
			} else {
				remote := trimRoot(f.root, path.Join(dir, it.Name))
				o := &Object{
					fs:       f,
					remote:   remote,
					id:       it.ID,
					name:     it.Name,
					size:     it.Size,
					modTime:  it.ModTime,
					mimeType: it.MimeType,
				}
				entries = append(entries, o)
			}
		}
		// Replace all children in cache atomically.
		allChildren := append(folders, items...)
		f.cache.replaceChildren(resolved.id, allChildren)

	case "folder":
		// List contents of a folder.
		items, err := GetItems(ctx, f.srv, f.hubID, resolved.id)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.IsContainer {
				remote := trimRoot(f.root, path.Join(dir, it.Name))
				d := fs.NewDir(remote, time.Time{})
				entries = append(entries, d)
			} else {
				remote := trimRoot(f.root, path.Join(dir, it.Name))
				o := &Object{
					fs:       f,
					remote:   remote,
					id:       it.ID,
					name:     it.Name,
					size:     it.Size,
					modTime:  it.ModTime,
					mimeType: it.MimeType,
				}
				entries = append(entries, o)
			}
		}
		// Replace all children in cache atomically.
		f.cache.replaceChildren(resolved.id, items)
	}

	fs.Infof(f, "List: dir=%q found %d entries", dir, len(entries))
	return entries, nil
}

// NewObject finds an Object at the remote path.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	fullPath := path.Join(f.root, remote)
	resolved, err := f.resolvePath(ctx, fullPath)
	if err != nil {
		return nil, err
	}
	if resolved.kind != "item" {
		return nil, fs.ErrorIsDir
	}

	details, err := GetItemDetails(ctx, f.srv, f.hubID, resolved.id)
	if err != nil {
		return nil, err
	}

	return &Object{
		fs:       f,
		remote:   remote,
		id:       resolved.id,
		altID:    resolved.altID,
		name:     details.Name,
		size:     parseSizeString(details.Size),
		modTime:  details.ModifiedOn,
		mimeType: details.MimeType,
	}, nil
}

// Put uploads a new file, or creates a new version if the file already exists.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	fullPath := path.Join(f.root, remote)
	dir, leaf := path.Split(fullPath)
	dir = strings.TrimSuffix(dir, "/")
	fs.Debugf(f, "Put: remote=%q leaf=%q size=%d", remote, leaf, src.Size())

	// Skip temp files from macOS safe-save — they'll be followed by a rename
	// to the real filename, which rclone handles via Update.
	if isTempName(leaf) || isTempName(path.Base(dir)) {
		return &Object{
			fs:      f,
			remote:  remote,
			name:    leaf,
			size:    src.Size(),
			modTime: time.Now(),
		}, nil
	}

	parentResolved, err := f.resolvePath(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("resolving parent directory: %w", err)
	}

	projectDM := parentResolved.projectDM
	if projectDM == "" {
		return nil, errors.New("cannot determine project for upload")
	}

	// Resolve the folder DM ID via the REST API by walking the folder path.
	folderDM, err := f.resolveFolderDMForPath(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("resolving folder DM ID: %w", err)
	}

	// Upload content to storage.
	storageID, err := f.createStorage(ctx, projectDM, folderDM, leaf)
	if err != nil {
		return nil, fmt.Errorf("creating storage: %w", err)
	}

	if err := f.uploadToStorage(ctx, storageID, in, src.Size()); err != nil {
		return nil, fmt.Errorf("uploading file: %w", err)
	}

	// Check if the file already exists — if so, create a new version instead.
	existing, _ := f.resolvePath(ctx, fullPath)
	if existing != nil && existing.kind != "hub" && existing.kind != "project" && existing.kind != "folder" {
		// File exists — resolve its DM item ID and create new version.
		fs.Debugf(f, "Put: file %q already exists, creating new version", leaf)
		itemDM, err := f.resolveItemDM(ctx, projectDM, folderDM, leaf)
		if err == nil && itemDM != "" {
			if err := f.createNextVersion(ctx, projectDM, itemDM, leaf, storageID); err != nil {
				return nil, fmt.Errorf("creating new version: %w", err)
			}
			f.cache.invalidate(parentResolved.id)
			return &Object{
				fs:      f,
				remote:  remote,
				id:      existing.id,
				altID:   itemDM,
				name:    leaf,
				size:    src.Size(),
				modTime: time.Now(),
			}, nil
		}
	}

	// New file — create first version.
	fs.Debugf(f, "Put: creating first version of %q", leaf)
	itemID, err := f.createFirstVersion(ctx, projectDM, folderDM, leaf, storageID)
	if err != nil {
		return nil, fmt.Errorf("creating item version: %w", err)
	}

	f.cache.invalidate(parentResolved.id)

	return &Object{
		fs:      f,
		remote:  remote,
		id:      itemID,
		name:    leaf,
		size:    src.Size(),
		modTime: time.Now(),
	}, nil
}

// isTempName detects macOS safe-save temp patterns like "file.sb-409cc62d-HQ8U9f".
func isTempName(name string) bool {
	return strings.Contains(name, ".sb-") || strings.HasPrefix(name, ".~") || strings.HasPrefix(name, "~$")
}

// Mkdir creates a directory (folder) at the given path.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	fullDir := path.Join(f.root, dir)
	segments := splitPath(fullDir)
	if len(segments) == 0 {
		return nil // root already exists
	}

	// Silently ignore temp directories from macOS safe-save.
	leaf := segments[len(segments)-1]
	if isTempName(leaf) {
		return nil
	}

	// Check if directory already exists.
	_, err := f.resolvePath(ctx, fullDir)
	if err == nil {
		return nil // already exists
	}

	// Resolve parent.
	parentPath := path.Dir(fullDir)
	parent, err := f.resolvePath(ctx, parentPath)
	if err != nil {
		return fmt.Errorf("resolving parent: %w", err)
	}

	if parent.projectDM == "" {
		return errors.New("cannot create folder: no project context")
	}

	// Resolve the parent folder DM ID via the REST API.
	parentFolderDM, err := f.resolveFolderDMForPath(ctx, parentPath)
	if err != nil {
		return fmt.Errorf("resolving parent folder DM ID: %w", err)
	}

	fs.Debugf(f, "Mkdir: creating folder %q in project=%q parent=%q", leaf, parent.projectDM, parentFolderDM)
	_, err = f.createFolder(ctx, parent.projectDM, parentFolderDM, leaf)
	if err != nil {
		return err
	}

	f.cache.invalidate(parent.id)
	return nil
}

// Rmdir removes an empty directory.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	// APS does not support folder deletion via the public API.
	// Return nil silently to avoid breaking macOS safe-save workflows
	// that create and delete temp directories.
	return nil
}

// resolveFolderDMForPath resolves an rclone directory path to a DM API folder ID
// by walking the folder tree via the REST Data Management API.
func (f *Fs) resolveFolderDMForPath(ctx context.Context, dir string) (string, error) {
	fs.Debugf(f, "resolveFolderDMForPath: dir=%q", dir)
	segments := splitPath(dir)
	if len(segments) < 2 {
		// At project level — find the project's root folder via DM API.
		resolved, err := f.resolvePath(ctx, dir)
		if err != nil {
			return "", err
		}
		if resolved.kind == "project" {
			topFolders, err := f.getTopFolders(ctx, f.hubDM, resolved.altID)
			if err != nil {
				return "", err
			}
			if len(topFolders) > 0 {
				return topFolders[0].ID, nil
			}
			return "", errors.New("no top folders found in project")
		}
		// Single segment that's a folder name — need project context.
		return "", errors.New("cannot resolve folder DM ID at this path level")
	}

	// segments[0] = project name, segments[1:] = folder path
	projectResolved, err := f.resolvePath(ctx, segments[0])
	if err != nil {
		return "", err
	}
	projectDM := projectResolved.altID
	folderNames := segments[1:]

	return f.resolveFolderDMPath(ctx, f.hubDM, projectDM, folderNames)
}

// trimRoot removes the root prefix from a path to produce a relative remote path.
func trimRoot(root, fullPath string) string {
	if root == "" {
		return fullPath
	}
	rel := strings.TrimPrefix(fullPath, root)
	return strings.TrimPrefix(rel, "/")
}

// parseSizeString parses a size string (e.g. "12345") to int64.
func parseSizeString(s string) int64 {
	var size int64
	fmt.Sscanf(s, "%d", &size)
	return size
}

// ChangeNotify polls for server-side changes and calls the notify function
// with paths that have changed. This enables --poll-interval support.
//
// The implementation snapshots directory listings and compares them on each
// poll cycle. When items are added, removed, or modified, the parent directory
// path is reported as changed.
func (f *Fs) ChangeNotify(ctx context.Context, notifyFunc func(string, fs.EntryType), pollIntervalChan <-chan time.Duration) {
	go func() {
		var ticker *time.Ticker
		var tickerC <-chan time.Time
		snapshots := make(map[string]dirSnapshot)

		for {
			select {
			case <-ctx.Done():
				if ticker != nil {
					ticker.Stop()
				}
				return

			case interval, ok := <-pollIntervalChan:
				if !ok {
					// Channel closed — stop polling.
					if ticker != nil {
						ticker.Stop()
					}
					return
				}
				if ticker != nil {
					ticker.Stop()
					tickerC = nil
				}
				if interval == 0 {
					// Pause polling.
					continue
				}
				ticker = time.NewTicker(interval)
				tickerC = ticker.C

			case <-tickerC:
				fs.Infof(f, "ChangeNotify: poll cycle starting")
				f.pollForChanges(ctx, notifyFunc, snapshots)
			}
		}
	}()
}

// dirSnapshot stores the last known state of a directory for change detection.
type dirSnapshot struct {
	entries map[string]int64 // name -> size (items) or -1 (directories)
}

// pollForChanges checks all cached directories for changes.
func (f *Fs) pollForChanges(ctx context.Context, notifyFunc func(string, fs.EntryType), snapshots map[string]dirSnapshot) {
	// Poll the hub root (projects).
	f.pollDir(ctx, "", notifyFunc, snapshots)

	// Poll each known project's top-level contents.
	projects, err := GetProjects(ctx, f.srv, f.hubID)
	if err != nil {
		fs.Debugf(f, "ChangeNotify: error listing projects: %v", err)
		return
	}

	for _, p := range projects {
		projDir := trimRoot(f.root, p.Name)
		f.pollDir(ctx, projDir, notifyFunc, snapshots)
	}
}

// pollDir lists a single directory and compares against the snapshot.
// If changes are detected, notifyFunc is called for the changed entries.
func (f *Fs) pollDir(ctx context.Context, dir string, notifyFunc func(string, fs.EntryType), snapshots map[string]dirSnapshot) {
	entries, err := f.List(ctx, dir)
	if err != nil {
		return
	}

	// Build current state.
	current := make(map[string]int64, len(entries))
	for _, entry := range entries {
		switch e := entry.(type) {
		case fs.Directory:
			current[e.Remote()] = -1
		case fs.Object:
			current[e.Remote()] = e.Size()
		}
	}

	// Compare with previous snapshot.
	prev, hasPrev := snapshots[dir]
	if hasPrev {
		changed := false
		// Check for new or modified entries.
		for name, size := range current {
			oldSize, existed := prev.entries[name]
			if !existed || oldSize != size {
				changed = true
				fs.Debugf(f, "ChangeNotify: changed entry=%q existed=%v", name, existed)
				if size == -1 {
					notifyFunc(name, fs.EntryDirectory)
				} else {
					notifyFunc(name, fs.EntryObject)
				}
			}
		}
		// Check for deleted entries.
		for name, size := range prev.entries {
			if _, exists := current[name]; !exists {
				changed = true
				fs.Debugf(f, "ChangeNotify: deleted entry=%q", name)
				if size == -1 {
					notifyFunc(name, fs.EntryDirectory)
				} else {
					notifyFunc(name, fs.EntryObject)
				}
			}
		}
		if changed {
			// Also notify the parent directory itself.
			fs.Infof(f, "ChangeNotify: changes detected in dir=%q", dir)
			notifyFunc(dir, fs.EntryDirectory)
		}
	}

	// Update snapshot.
	snapshots[dir] = dirSnapshot{entries: current}
}

// Check interfaces are satisfied.
var (
	_ fs.Fs             = (*Fs)(nil)
	_ fs.Mover          = (*Fs)(nil)
	_ fs.DirMover       = (*Fs)(nil)
	_ fs.ChangeNotifier = (*Fs)(nil)
)
