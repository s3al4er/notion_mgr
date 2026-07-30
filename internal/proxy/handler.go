package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"notion-manager/internal/netutil"
)

// Default and maximum page sizes for /admin/accounts pagination.
// Default is small to keep dashboard payloads quick; the cap prevents
// callers from accidentally requesting the whole pool through the
// paginated path.
const (
	defaultAccountsPageSize = 50
	maxAccountsPageSize     = 500
)

const publicModelCreatedAt = int64(1735689600)

type publicModelResponse struct {
	Object string        `json:"object"`
	Data   []publicModel `json:"data"`
}

type publicModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// HandleHealth returns an HTTP handler for the /health endpoint
func HandleHealth(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"status":    "ok",
			"accounts":  pool.Count(),
			"available": pool.AvailableCount(),
			"quota":     pool.GetQuotaSummary(),
		}
		json.NewEncoder(w).Encode(resp)
	}
}

// HandlePublicModels returns an OpenAI-compatible models list for API clients.
func HandlePublicModels(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, `{"error":{"message":"method not allowed","type":"invalid_request_error"}}`, http.StatusMethodNotAllowed)
			return
		}

		resp := publicModelResponse{
			Object: "list",
			Data:   buildPublicModels(pool.AllModels()),
		}
		json.NewEncoder(w).Encode(resp)
	}
}

func buildPublicModels(models []ModelEntry) []publicModel {
	seen := make(map[string]bool, len(models))
	items := make([]publicModel, 0, len(models))
	for _, model := range models {
		id := publicModelID(model)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		items = append(items, publicModel{
			ID:      id,
			Object:  "model",
			Created: publicModelCreatedAt,
			OwnedBy: "notion-manager",
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items
}

func publicModelID(model ModelEntry) string {
	if normalized := normalizeModelName(model.Name); normalized != "" {
		return normalized
	}
	return friendlyModelNameByInternalID(model.ID)
}

func friendlyModelNameByInternalID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return ""
	}

	snap := SnapshotModelMap()
	candidates := make([]string, 0, 1)
	for friendly, internalID := range snap {
		if internalID == trimmed {
			candidates = append(candidates, friendly)
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	sort.Strings(candidates)
	return candidates[0]
}

// HandleAdminAccounts returns detailed account info including models, quota, and status.
//
// Query parameters (all optional, dashboard-friendly):
//   - q          : case-insensitive substring filter on email/name/plan/space.
//   - page       : 0-based page index. Defaults to 0.
//   - page_size  : max entries to return; clamped to [1, maxAccountsPageSize].
//
// When ANY of those parameters are present we apply the same sort the
// dashboard previously did client-side, filter, then slice — and add
// `page`, `page_size`, `filtered_total` fields to the response. Without
// them the response keeps its historical shape (the full unsorted list)
// so older scripts and integrations remain happy. The pool-wide
// `summary` block is added unconditionally because it's purely additive
// and lets the dashboard render headline cards without iterating the
// full account list.
func HandleAdminAccounts(pool *AccountPool, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		q := strings.TrimSpace(r.URL.Query().Get("q"))
		pageStr := r.URL.Query().Get("page")
		sizeStr := r.URL.Query().Get("page_size")
		paginated := pageStr != "" || sizeStr != "" || q != ""

		all := pool.GetAccountDetails()
		resp := map[string]interface{}{
			"total":     pool.Count(),
			"available": pool.AvailableCount(),
			"models":    pool.AllModels(),
			"refresh":   pool.GetRefreshStatus(),
			"summary":   summarizeAccounts(all),
		}

		if !paginated {
			// Backward-compatible path: hand back the full unsorted
			// list so existing scripts/integrations keep working.
			resp["accounts"] = all
			json.NewEncoder(w).Encode(resp)
			return
		}

		filtered := filterAccountDetails(all, q)
		sortAccountDetails(filtered)

		page, _ := strconv.Atoi(pageStr)
		if page < 0 {
			page = 0
		}
		size, _ := strconv.Atoi(sizeStr)
		if size <= 0 {
			size = defaultAccountsPageSize
		}
		if size > maxAccountsPageSize {
			size = maxAccountsPageSize
		}

		resp["accounts"] = paginateAccounts(filtered, page, size)
		resp["page"] = page
		resp["page_size"] = size
		resp["filtered_total"] = len(filtered)
		json.NewEncoder(w).Encode(resp)
	}
}

// HandleAdminStats returns aggregated Token usage statistics for the
// dashboard. It only requires a valid dashboard session — same auth
// surface as /admin/accounts. The response shape is documented on
// UsageSnapshot.
func HandleAdminStats(stats *UsageStats, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}
		if stats == nil {
			stats = GlobalUsageStats()
		}
		snap := stats.Snapshot(5)
		json.NewEncoder(w).Encode(snap)
	}
}

// HandleAdminRefresh handles GET (status) and POST (trigger) for quota refresh
func HandleAdminRefresh(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(pool.GetRefreshStatus())
		case "POST":
			started := pool.TriggerRefresh(accountsDir)
			resp := map[string]interface{}{
				"started": started,
			}
			if !started {
				resp["message"] = "refresh already in progress"
			}
			json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// HandleAdminModels returns the current model mapping (friendly name -> Notion internal ID)
func HandleAdminModels(pool *AccountPool, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}
		resp := map[string]interface{}{
			"model_map":        SnapshotModelMap(),
			"available_models": pool.AllModels(),
		}
		json.NewEncoder(w).Encode(resp)
	}
}

// HandleAdminModelAliases manages the model name aliases (model_map).
// GET: list all aliases
// PUT: replace all aliases
func HandleAdminModelAliases(configPath string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"aliases": SnapshotModelMap(),
			})

		case "PUT":
			var body struct {
				Aliases map[string]string `json:"aliases"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			if body.Aliases == nil {
				http.Error(w, `{"error":"aliases map is required"}`, http.StatusBadRequest)
				return
			}

			// Apply to in-memory map immediately
			ReplaceModelMap(body.Aliases)

			// Persist to config.yaml
			if configPath != "" {
				persistModelMap(configPath)
			}

			log.Printf("[models] model aliases updated (%d entries)", len(body.Aliases))
			json.NewEncoder(w).Encode(map[string]interface{}{
				"aliases": SnapshotModelMap(),
			})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// HandleAdminSettings handles GET (read) and PUT (update) for dashboard-controlled settings.
// Settings are persisted to config.yaml using YAML node manipulation to preserve comments.
func HandleAdminSettings(configPath string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Require dashboard session (admin password auth)
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"enable_web_search":         AppConfig.WebSearchEnabled(),
				"enable_workspace_search":   AppConfig.WorkspaceSearchEnabled(),
				"ask_mode_default":          AppConfig.AskModeDefault(),
				"disable_notion_prompt":     AppConfig.Proxy.DisableNotionPrompt,
				"debug_logging":             AppConfig.Server.DebugLogging,
				"notion_proxy":              AppConfig.NotionProxyURL(),
				"keyless_endpoint":          AppConfig.KeylessEndpoint(),
				"require_api_key_for_models": AppConfig.ModelsRequireApiKey(),
				"theme_bg_color":            AppConfig.Theme.BgColor,
				"theme_text_color":          AppConfig.Theme.TextColor,
				"theme_sidebar_color":       AppConfig.Theme.SidebarColor,
			})

		case "PUT":
			var body struct {
				EnableWebSearch        *bool   `json:"enable_web_search"`
				EnableWorkspaceSearch  *bool   `json:"enable_workspace_search"`
				AskModeDefault         *bool   `json:"ask_mode_default"`
				DebugLogging           *bool   `json:"debug_logging"`
				NotionProxy            *string `json:"notion_proxy"`
				KeylessEndpoint        *bool   `json:"keyless_endpoint"`
				RequireApiKeyForModels *bool   `json:"require_api_key_for_models"`
				ThemeBgColor           *string `json:"theme_bg_color"`
				ThemeTextColor         *string `json:"theme_text_color"`
				ThemeSidebarColor      *string `json:"theme_sidebar_color"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}

			changed := false
			rebuildTransport := false
			if body.EnableWebSearch != nil {
				AppConfig.Proxy.EnableWebSearch = body.EnableWebSearch
				changed = true
				log.Printf("[settings] enable_web_search → %v", *body.EnableWebSearch)
			}
			if body.EnableWorkspaceSearch != nil {
				AppConfig.Proxy.EnableWorkspaceSearch = body.EnableWorkspaceSearch
				changed = true
				log.Printf("[settings] enable_workspace_search → %v", *body.EnableWorkspaceSearch)
			}
			if body.AskModeDefault != nil {
				AppConfig.Proxy.AskModeDefault = body.AskModeDefault
				changed = true
				log.Printf("[settings] ask_mode_default → %v", *body.AskModeDefault)
			}
			if body.DebugLogging != nil {
				AppConfig.Server.DebugLogging = *body.DebugLogging
				SetDebugLoggingEnabled(*body.DebugLogging)
				changed = true
				log.Printf("[settings] debug_logging → %v", *body.DebugLogging)
			}
			if body.NotionProxy != nil {
				next := strings.TrimSpace(*body.NotionProxy)
				if next != "" {
					if err := netutil.ValidateProxyURL(next); err != nil {
						// Surface scheme/format errors immediately so the
						// dashboard can roll back the input field instead
						// of waiting for the next dial to fail.
						http.Error(w, `{"error":"unsupported proxy scheme (want http/https/socks5)"}`, http.StatusBadRequest)
						return
					}
				}
				if AppConfig.Proxy.NotionProxy != next {
					AppConfig.Proxy.NotionProxy = next
					changed = true
					rebuildTransport = true
					if next == "" {
						log.Printf("[settings] notion_proxy cleared (direct dial)")
					} else {
						log.Printf("[settings] notion_proxy → %s", next)
					}
				}
			}

			if body.KeylessEndpoint != nil {
				AppConfig.Server.KeylessEndpoint = *body.KeylessEndpoint
				changed = true
				log.Printf("[settings] keyless_endpoint → %v", *body.KeylessEndpoint)
			}
			if body.RequireApiKeyForModels != nil {
				AppConfig.Server.RequireApiKeyForModels = *body.RequireApiKeyForModels
				changed = true
				log.Printf("[settings] require_api_key_for_models → %v", *body.RequireApiKeyForModels)
			}
			if body.ThemeBgColor != nil {
				AppConfig.Theme.BgColor = *body.ThemeBgColor
				changed = true
			}
			if body.ThemeTextColor != nil {
				AppConfig.Theme.TextColor = *body.ThemeTextColor
				changed = true
			}
			if body.ThemeSidebarColor != nil {
				AppConfig.Theme.SidebarColor = *body.ThemeSidebarColor
				changed = true
			}

			// Persist to config.yaml
			if changed && configPath != "" {
				persistSearchSettings(configPath)
			}

			// Drop idle pooled connections so the next notion dial picks
			// up the new upstream proxy. Active in-flight requests
			// continue on their existing connection until completion.
			if rebuildTransport {
				RebuildChromeTransport()
			}

			json.NewEncoder(w).Encode(map[string]interface{}{
				"enable_web_search":         AppConfig.WebSearchEnabled(),
				"enable_workspace_search":   AppConfig.WorkspaceSearchEnabled(),
				"ask_mode_default":          AppConfig.AskModeDefault(),
				"disable_notion_prompt":     AppConfig.Proxy.DisableNotionPrompt,
				"debug_logging":             AppConfig.Server.DebugLogging,
				"notion_proxy":              AppConfig.NotionProxyURL(),
				"keyless_endpoint":          AppConfig.KeylessEndpoint(),
				"require_api_key_for_models": AppConfig.ModelsRequireApiKey(),
				"theme_bg_color":            AppConfig.Theme.BgColor,
				"theme_text_color":          AppConfig.Theme.TextColor,
				"theme_sidebar_color":       AppConfig.Theme.SidebarColor,
			})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// persistSearchSettings writes the current dashboard settings back to config.yaml.
func persistSearchSettings(configPath string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("[settings] failed to read %s: %v", configPath, err)
		return
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || root.Kind == 0 {
		log.Printf("[settings] failed to parse %s: %v", configPath, err)
		return
	}

	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		mapping := root.Content[0]
		proxyNode := getOrCreateYAMLMapping(mapping, "proxy")
		setYAMLBool(proxyNode, "enable_web_search", AppConfig.WebSearchEnabled())
		setYAMLBool(proxyNode, "enable_workspace_search", AppConfig.WorkspaceSearchEnabled())
		setYAMLBool(proxyNode, "ask_mode_default", AppConfig.AskModeDefault())
		setYAMLString(proxyNode, "notion_proxy", AppConfig.Proxy.NotionProxy)

		serverNode := getOrCreateYAMLMapping(mapping, "server")
		setYAMLBool(serverNode, "debug_logging", AppConfig.Server.DebugLogging)
		setYAMLBool(serverNode, "keyless_endpoint", AppConfig.Server.KeylessEndpoint)
		setYAMLBool(serverNode, "require_api_key_for_models", AppConfig.Server.RequireApiKeyForModels)

		themeNode := getOrCreateYAMLMapping(mapping, "theme")
		setYAMLString(themeNode, "bg_color", AppConfig.Theme.BgColor)
		setYAMLString(themeNode, "text_color", AppConfig.Theme.TextColor)
		setYAMLString(themeNode, "sidebar_color", AppConfig.Theme.SidebarColor)
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		log.Printf("[settings] failed to marshal config: %v", err)
		return
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		log.Printf("[settings] failed to write %s: %v", configPath, err)
	}
}

func getOrCreateYAMLMapping(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			node := mapping.Content[i+1]
			if node.Kind != yaml.MappingNode {
				node.Kind = yaml.MappingNode
				node.Tag = "!!map"
				node.Content = nil
			}
			return node
		}
	}

	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		node,
	)
	return node
}

// setYAMLBool sets or creates a boolean field in a YAML mapping node
func setYAMLBool(mapping *yaml.Node, key string, value bool) {
	valStr := "false"
	if value {
		valStr = "true"
	}
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = valStr
			mapping.Content[i+1].Tag = "!!bool"
			return
		}
	}
	// Key not found — append it
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: valStr, Tag: "!!bool"},
	)
}

// setYAMLString sets or creates a string field in a YAML mapping node.
// Empty values are still written explicitly so the dashboard's "clear
// proxy" action persists across restarts (otherwise YAML would treat a
// missing key as "use default", which here is also "" — equivalent in
// practice but unfriendly for diffs/audits).
func setYAMLString(mapping *yaml.Node, key, value string) {
	style := yaml.Style(0)
	if value == "" {
		style = yaml.DoubleQuotedStyle
	}
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = value
			mapping.Content[i+1].Tag = "!!str"
			mapping.Content[i+1].Style = style
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: "!!str", Style: style},
	)
}

// HandleAdminApiKeys manages the API keys.
// GET: list all keys (masked)
// POST: generate a new key
// DELETE: remove a key
func HandleAdminApiKeys(configPath string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		switch r.Method {
		case "GET":
			keys := AppConfig.GetApiKeys()
			masked := make([]map[string]string, len(keys))
			for i, k := range keys {
				maskedKey := maskApiKey(k)
				masked[i] = map[string]string{
					"id":   fmt.Sprintf("key-%d", i),
					"key":  k,
					"masked": maskedKey,
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"keys": masked})

		case "POST":
			newKey := GenerateApiKey()
			AppConfig.Server.ApiKeys = append(AppConfig.Server.ApiKeys, newKey)
			if configPath != "" {
				persistApiKeys(configPath)
			}
			log.Printf("[api-keys] generated new key: %s", maskApiKey(newKey))
			json.NewEncoder(w).Encode(map[string]string{"key": newKey})

		case "DELETE":
			var body struct {
				Key string `json:"key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
				http.Error(w, `{"error":"key is required"}`, http.StatusBadRequest)
				return
			}
			if body.Key == AppConfig.Server.ApiKey {
				http.Error(w, `{"error":"cannot delete the primary API key"}`, http.StatusBadRequest)
				return
			}
			found := false
			newKeys := make([]string, 0, len(AppConfig.Server.ApiKeys))
			for _, k := range AppConfig.Server.ApiKeys {
				if k == body.Key {
					found = true
				} else {
					newKeys = append(newKeys, k)
				}
			}
			if !found {
				http.Error(w, `{"error":"key not found"}`, http.StatusNotFound)
				return
			}
			AppConfig.Server.ApiKeys = newKeys
			if configPath != "" {
				persistApiKeys(configPath)
			}
			log.Printf("[api-keys] deleted key: %s", maskApiKey(body.Key))
			json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func maskApiKey(key string) string {
	if len(key) <= 10 {
		return key[:min(len(key), 3)] + "•••"
	}
	return key[:7] + "•••" + key[len(key)-4:]
}

// HandleAdminAccountTokens returns token_v2 for all accounts.
func HandleAdminAccountTokens(pool *AccountPool, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		type tokenEntry struct {
			AccountID string `json:"account_id"`
			Email     string `json:"email"`
			Name      string `json:"name"`
			TokenV2   string `json:"token_v2"`
		}

		tokens := make([]tokenEntry, 0)
		pool.ForEach(func(acc *Account) {
			acc.mu.RLock()
			tokens = append(tokens, tokenEntry{
				AccountID: acc.AccountID,
				Email:     acc.UserEmail,
				Name:      acc.UserName,
				TokenV2:   acc.TokenV2,
			})
			acc.mu.RUnlock()
		})

		json.NewEncoder(w).Encode(map[string]interface{}{"tokens": tokens})
	}
}

// HandleChangePassword changes the admin password.
func HandleChangePassword(configPath string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		if !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		var body struct {
			OldPassword string `json:"old_password"`
			NewPassword string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if body.OldPassword == "" || body.NewPassword == "" {
			http.Error(w, `{"error":"old_password and new_password are required"}`, http.StatusBadRequest)
			return
		}
		if len(body.NewPassword) < 4 {
			http.Error(w, `{"error":"new password must be at least 4 characters"}`, http.StatusBadRequest)
			return
		}

		// Verify old password by comparing hash
		if !VerifyAdminPassword(auth.adminPasswordHash, body.OldPassword) {
			http.Error(w, `{"error":"old password is incorrect"}`, http.StatusUnauthorized)
			return
		}

		// Hash and save new password
		newHash := HashAdminPassword(body.NewPassword)
		auth.adminPasswordHash = newHash

		// Update AppConfig
		AppConfig.Server.AdminPassword = newHash

		// Persist to config.yaml
		if configPath != "" {
			persistAdminPassword(configPath, newHash)
		}

		log.Printf("[dashboard] admin password changed")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// persistApiKeys writes the current api_keys list to config.yaml.
func persistApiKeys(configPath string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("[api-keys] failed to read %s: %v", configPath, err)
		return
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || root.Kind == 0 {
		log.Printf("[api-keys] failed to parse %s: %v", configPath, err)
		return
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		mapping := root.Content[0]
		serverNode := getOrCreateYAMLMapping(mapping, "server")
		setYAMLStrings(serverNode, "api_keys", AppConfig.Server.ApiKeys)
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		log.Printf("[api-keys] failed to marshal config: %v", err)
		return
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		log.Printf("[api-keys] failed to write %s: %v", configPath, err)
	}
}

// persistAdminPassword writes the admin password hash to config.yaml.
func persistAdminPassword(configPath, hash string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("[settings] failed to read %s: %v", configPath, err)
		return
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || root.Kind == 0 {
		log.Printf("[settings] failed to parse %s: %v", configPath, err)
		return
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		mapping := root.Content[0]
		serverNode := getOrCreateYAMLMapping(mapping, "server")
		for i := 0; i < len(serverNode.Content)-1; i += 2 {
			if serverNode.Content[i].Value == "admin_password" {
				serverNode.Content[i+1].Value = hash
				serverNode.Content[i+1].Tag = "!!str"
				serverNode.Content[i+1].Style = yaml.DoubleQuotedStyle
				goto write
			}
		}
		serverNode.Content = append(serverNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "admin_password"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: hash, Tag: "!!str", Style: yaml.DoubleQuotedStyle},
		)
	}
write:
	out, err := yaml.Marshal(&root)
	if err != nil {
		log.Printf("[settings] failed to marshal config: %v", err)
		return
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		log.Printf("[settings] failed to write %s: %v", configPath, err)
	}
}

// persistModelMap writes the current model alias map to config.yaml.
func persistModelMap(configPath string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("[models] failed to read %s: %v", configPath, err)
		return
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || root.Kind == 0 {
		log.Printf("[models] failed to parse %s: %v", configPath, err)
		return
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		mapping := root.Content[0]
		modelMap := SnapshotModelMap()
		// Find or create model_map node
		for i := 0; i < len(mapping.Content)-1; i += 2 {
			if mapping.Content[i].Value == "model_map" {
				mapNode := mapping.Content[i+1]
				mapNode.Kind = yaml.MappingNode
				mapNode.Tag = "!!map"
				mapNode.Content = nil
				// Sort keys for deterministic output
				keys := make([]string, 0, len(modelMap))
				for k := range modelMap {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					mapNode.Content = append(mapNode.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Value: k, Tag: "!!str"},
						&yaml.Node{Kind: yaml.ScalarNode, Value: modelMap[k], Tag: "!!str"},
					)
				}
				goto writeModelMap
			}
		}
		// model_map key not found, create it
		mapNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for k, v := range modelMap {
			mapNode.Content = append(mapNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: k, Tag: "!!str"},
				&yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"},
			)
		}
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "model_map"},
			mapNode,
		)
	}
writeModelMap:
	out, err := yaml.Marshal(&root)
	if err != nil {
		log.Printf("[models] failed to marshal config: %v", err)
		return
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		log.Printf("[models] failed to write %s: %v", configPath, err)
	}
}

// isFreePlan returns true if the account is on a free plan where basic credits (200 lifetime)
// never reset. Paid plans (plus, business, enterprise) have monthly premium credits that reset.
func isFreePlan(acc *Account) bool {
	quota := acc.quotaInfoSnapshot()
	if quota != nil && (quota.HasPremium || quota.PremiumLimit > 0 || quota.PremiumBalance > 0) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(acc.PlanType)) {
	case "personal", "free", "":
		return true
	default:
		// For team plans, check if they actually have a paid subscription
		// by looking at quota info — if no premium credits exist, treat as free.
		if quota != nil && !quota.HasPremium {
			return true
		}
		return false
	}
}
