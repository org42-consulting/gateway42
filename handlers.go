package main

// HTTP handlers for the admin web UI: session-cookie authenticated, HTML
// rendering, CSRF-protected form posts.
//
// The OpenAI-compatible /v1 surface lives in api_handlers.go. Anything added
// here should render a page or mutate admin state; anything that answers an SDK
// client belongs there.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
)

// ─────────────────────────────── Index / login ────────────────────────────────

// handleIndex renders the login page
func handleIndex(w http.ResponseWriter, r *http.Request) {
	renderPage(w, "login", LoginData{BaseData: newBaseData(w, r)})
}

// handleLogout logs out the admin user
func handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	sess.Values["admin"] = false
	if !saveSession(w, r, sess) {
		// The browser still holds a cookie asserting admin. Redirecting to the
		// login page would tell the operator they are logged out when they are
		// not, which is the one outcome worth a 500 to avoid.
		http.Error(w, "could not end session", http.StatusInternalServerError)
		return
	}
	slog.Info("Admin logged out")
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleAdminLogin processes admin login requests
func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	password := r.FormValue("password")

	// Before the derive function, not after it: 260,000 PBKDF2 rounds is
	// exactly the work this guard exists to skip (see loginGuard in auth.go).
	if ok, retryAfter := adminLogin.allow(time.Now()); !ok {
		// Round up once so the header and the message cannot disagree.
		secs := int((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		slog.Warn("admin login throttled", "ip", clientIP(r), "retry_after_s", secs)
		// A plain 429 rather than this handler's usual flash-and-redirect: the
		// browser does not follow a redirect on a 429, so a flash would sit in
		// the session until some later page render and surface as stale advice
		// on an unrelated request — possibly the successful login that follows.
		http.Error(w, fmt.Sprintf("Too many failed attempts. Try again in %s.",
			time.Duration(secs)*time.Second), http.StatusTooManyRequests)
		return
	}

	admin, err := getAdmin()
	if err != nil {
		// The gateway's own fault, not a wrong password. Counting it would let a
		// database hiccup spend the attempt budget and answer a server-side
		// problem with a 429.
		slog.Error("getAdmin", "err", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if admin == nil || !verifyPassword(admin.PasswordHash, password) {
		if lockout := adminLogin.failed(time.Now()); lockout > 0 {
			slog.Warn("admin login locked out", "ip", clientIP(r), "lockout", lockout)
		}
		addFlash(w, r, "error", "Invalid password")
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	// The password was right, so any backlog of near-misses is cleared before
	// the session work below — which can itself fail, and a failed cookie save
	// is not a reason to hold the next attempt against the caller.
	adminLogin.succeeded()

	sess := getSession(r)
	sess.Values["admin"] = true
	sess.Options.MaxAge = cfg.SessionTTL
	if !saveSession(w, r, sess) {
		// Redirecting to the panel would bounce straight back to the login page,
		// since the cookie granting access was never issued. Say so instead.
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	slog.Info("Admin logged in")
	// A login that just verified is the one moment the gateway knows the
	// account is still at factory settings: the submitted password matched
	// the stored hash, so if it is the well-known default, the hash is the
	// default's. Flip the flag here rather than at startup — an operator who
	// pre-set a different password is never nagged, and one who left the
	// default is nagged from their very first login.
	if isDefaultPassword(password) {
		SetMustChangePassword(true)
		addFlash(w, r, "error",
			"You logged in with the default admin password. Change it in Change Admin Password on the Settings page — until you do, the rest of the dashboard stays locked.")
		http.Redirect(w, r, changePasswordPage, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// ─────────────────────────────── Admin panel ──────────────────────────────────

// handleAdminPanel renders the admin panel page
func handleAdminPanel(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !requirePasswordChanged(w, r) {
		return
	}
	users, err := getAllUsers()
	if err != nil {
		slog.Error("getAllUsers", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}

	engines := cachedEngines()
	enginesURLs := getEngineURLs(engines)
	enginesStatus := make(map[string]bool)
	enginesModels := make(map[string][]string)

	// Render from whatever the probe cache holds; one bounded warm only on a
	// completely cold cache. The old warm-then-probe loop advertised a "hard
	// 3s budget" but could take far longer: probeEngineCached blocks on a probe
	// another request started earlier, and that wait had no deadline. Expired
	// data still renders here, with the refresh running behind it.
	snap, stale := cachedProbeSnapshot()
	if len(snap) == 0 {
		warmCtx, warmCancel := context.WithTimeout(r.Context(), 3*time.Second)
		warmProbeCache(warmCtx)
		warmCancel()
		snap, stale = cachedProbeSnapshot()
	}
	if stale {
		go warmProbeCache(context.Background())
	}

	for _, e := range engines {
		pd := snap[e.ID]
		enginesStatus[fmt.Sprintf("%d", e.ID)] = pd.status
		enginesModels[fmt.Sprintf("%d", e.ID)] = pd.models
	}

	// Window comes from a query-string link, so it is validated against
	// usageWindows rather than parsed — the value reaches a SQL comparison.
	window := usageWindowFor(r.URL.Query().Get("usage"))
	since := time.Now().UTC().Add(-24 * time.Hour * time.Duration(usageWindows[window].days)).Format(time.RFC3339)
	usage, err := getUsageByClient(since)

	var panel *UsagePanel
	if err != nil {
		// Leave the panel nil rather than rendering an empty one: a nil panel
		// says "usage unavailable", where an empty one would claim nobody
		// spent any tokens. The engine-status table on this page is the
		// operational signal, and losing it to a failed aggregate would be the
		// worse trade, so the page still renders.
		slog.Error("getUsageByClient", "err", err, "window", window)
	} else {
		panel = buildUsagePanel(usage, window, len(users))
	}

	renderPage(w, "dashboard", DashboardData{
		BaseData:     newBaseData(w, r),
		Users:        users,
		Engines:      engines,
		EngineURLs:   enginesURLs,
		EngineStatus: enginesStatus,
		EngineModels: enginesModels,
		Usage:        panel,
		// Nil — not an empty slice — when no user has a budget: the template
		// tests {{if .QuotaRows}}, and a fully unlimited fleet renders no
		// panel at all. buildQuotaRows also returns nil on a pre-migration
		// database (the db.go migration adds users.quota_daily_tokens at
		// startup), so the panel stays empty instead of the dashboard
		// erroring.
		QuotaRows: buildQuotaRows(users),
	})
}

// ─────────────────────────────── Settings page ────────────────────────────────

func handleAdminSettingsPage(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	// No requirePasswordChanged here, on purpose: this page hosts the
	// Change Admin Password form and is changePasswordPage itself. Guarding
	// it would turn the lockout into a redirect loop with no exit — the flag
	// clears only in handleChangePassword, reachable only from this form.
	engines := cachedEngines()
	engineURLs := getEngineURLs(engines)
	engineStatus := make(map[string]bool)
	engineModelDetails := make(map[string][]ModelDetail)

	// Same peek-first policy as the dashboard: render cached state, refresh
	// behind the response, block only on a cold cache.
	snap, stale := cachedProbeSnapshot()
	if len(snap) == 0 {
		warmCtx, warmCancel := context.WithTimeout(r.Context(), 3*time.Second)
		warmProbeCache(warmCtx)
		warmCancel()
		snap, stale = cachedProbeSnapshot()
	}
	if stale {
		go warmProbeCache(context.Background())
	}

	for _, e := range engines {
		pd := snap[e.ID]
		engineStatus[fmt.Sprintf("%d", e.ID)] = pd.status
		details := pd.details
		// oMLX models are managed here (load, unload, delete), so the page
		// shows which are loaded — a live, bounded read of /v1/models/status.
		if a, ok := cachedAdapter(e.ID).(*OpenAICompatAdapter); ok && isOMLX(a) && pd.status {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			details = withOMLXStatus(ctx, a, details)
			cancel()
		}
		engineModelDetails[fmt.Sprintf("%d", e.ID)] = details
	}

	renderPage(w, "settings", SettingsData{
		BaseData:           newBaseData(w, r),
		Engines:            engines,
		EngineURLs:         engineURLs,
		EngineStatus:       engineStatus,
		EngineModelDetails: engineModelDetails,
		SearchResults:      []ModelDetail{}, // Will be populated by search
	})
}

func handleEngineTest(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	engines := cachedEngines()
	if len(engines) == 0 {
		// Only saved engines are tested. The button sits in the add form, so
		// say that, rather than leave the admin wondering why the engine they
		// just typed in "is not configured".
		addFlash(w, r, "error", "No engines saved yet. Test Saved Engines only checks engines already added — click Add Engine first, then test.")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	var results []string
	for _, e := range engines {
		adapter := cachedAdapter(e.ID)
		if adapter == nil {
			results = append(results, fmt.Sprintf("%s: error (cached adapter missing)", e.Name))
			continue
		}
		status, err := adapter.Status()
		if err != nil {
			results = append(results, fmt.Sprintf("%s: failed (%v)", e.Name, err))
			continue
		}
		if status {
			results = append(results, fmt.Sprintf("%s: OK (%s)", e.Name, e.Type))
		} else {
			results = append(results, fmt.Sprintf("%s: not responding", e.Name))
		}
	}
	msg := strings.Join(results, "; ")
	if len(results) == 1 && strings.Contains(results[0], "OK") {
		addFlash(w, r, "success", results[0])
	} else {
		addFlash(w, r, "info", msg)
	}
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

func handleEngineSettings(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/settings-page") {
		return
	}

	engineName := strings.TrimSpace(r.FormValue("engine_name"))
	apiKey := r.FormValue("api_key")
	editIDStr := r.FormValue("engine_id")

	engineType, baseURL, portInt, msg := engineFormFields(r)
	if msg != "" {
		addFlash(w, r, "error", msg)
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	// Determine if editing existing or creating new.
	isEdit := editIDStr != ""
	var editID int
	if isEdit {
		var err error
		editID, err = strconv.Atoi(editIDStr)
		if err != nil {
			addFlash(w, r, "error", "Invalid engine ID")
			http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
			return
		}
	}

	engines, err := getEngines()
	if err != nil {
		slog.Error("getEngines", "err", err)
		addFlash(w, r, "error", "Could not read the current engine list; nothing was changed")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	if isEdit {
		// Update existing engine.
		found := false
		for i := range engines {
			if engines[i].ID == editID {
				applyEngineEdit(&engines[i], engineType, engineName, baseURL, portInt, apiKey)
				found = true
				break
			}
		}
		if !found {
			addFlash(w, r, "error", "Engine not found")
			http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
			return
		}
	} else {
		if engineName == "" {
			addFlash(w, r, "error", "Engine name is required")
			http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
			return
		}
		newID := 1
		for _, e := range engines {
			if e.ID >= newID {
				newID = e.ID + 1
			}
		}
		// Named ec, not cfg: cfg is the package-level Config global and
		// shadowing it here silently breaks every later cfg.* reference.
		ec := EngineConfig{
			ID:      newID,
			Name:    engineName,
			Type:    engineType,
			BaseURL: baseURL,
		}
		ec.APIKey = apiKey
		engines = append(engines, ec)
		editID = newID
	}

	if err := saveEngines(engines); err != nil {
		slog.Error("saveEngines", "err", err)
		addFlash(w, r, "error", "Failed to save engine config")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	action := "created"
	if isEdit {
		action = "updated"
	}
	slog.Info("Engine "+action, "id", editID, "type", engineType, "url", baseURL)
	addFlash(w, r, "success", "Engine "+action)
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

// applyEngineEdit merges a submitted edit form into an existing engine.
//
// Blank fields mean "keep", not "clear", for the two values the form cannot
// show back: the name is optional on edit, and the API key is never rendered
// into the page (it is a credential), so the admin always submits it empty
// unless they are replacing it. Overwriting with "" would silently strip an
// OpenAI-compatible engine's key on every unrelated edit.
//
// Fields that belong only to the other engine type are cleared on a type
// change, so a stale port or key cannot resurface if the type is switched back.
// engineFormFields reads and validates the engine form fields that saving and
// testing share. msg is the error to show the admin, empty when the fields are
// valid. Both engine types take their port in the base URL, so port is 0; it
// is kept in the signature for the callers that still pass it on.
func engineFormFields(r *http.Request) (engineType, baseURL string, port int, msg string) {
	engineType = r.FormValue("engine_type")
	baseURL = strings.TrimSpace(r.FormValue("base_url"))

	if engineType != EngineOMLX && engineType != EngineOpenAICompat {
		return "", "", 0, "Invalid engine type"
	}
	if baseURL == "" {
		return "", "", 0, "Base URL is required"
	}
	return engineType, baseURL, 0, ""
}

// handleEngineTestDraft tests the engine settings typed into the form before
// they are saved. The page calls it with fetch and shows the answer next to
// the button, so the typed values stay in the form whatever the outcome.
func handleEngineTestDraft(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	reply := func(ok bool, msg string) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": ok, "message": msg})
	}
	if !isAdminSession(r) {
		w.WriteHeader(http.StatusUnauthorized)
		reply(false, "Session expired; log in again")
		return
	}
	if err := r.ParseForm(); err != nil {
		reply(false, "Could not read the form")
		return
	}
	engineType, baseURL, port, msg := engineFormFields(r)
	if msg != "" {
		reply(false, msg)
		return
	}

	// Build the config exactly as saving would. When editing, start from the
	// saved engine: a blank API key means "keep the current one", so the test
	// must use that key too, or it would fail where the saved engine works.
	ec := EngineConfig{Name: "draft"}
	if id, err := strconv.Atoi(r.FormValue("engine_id")); err == nil {
		for _, e := range cachedEngines() {
			if e.ID == id {
				ec = e
				break
			}
		}
	}
	applyEngineEdit(&ec, engineType, "", baseURL, port, r.FormValue("api_key"))

	adapter, err := newEngine(ec)
	if err != nil {
		reply(false, err.Error())
		return
	}
	up, err := adapter.Status()
	if err != nil {
		reply(false, "Connection failed: "+err.Error())
		return
	}
	if !up {
		reply(false, "The engine answered, but does not report itself as ready")
		return
	}
	models, err := adapter.ListModels()
	if err != nil {
		reply(true, "Connected, but listing models failed: "+err.Error())
		return
	}
	reply(true, fmt.Sprintf("Connected. %d model(s) available.", len(models)))
}

func applyEngineEdit(e *EngineConfig, engineType, name, baseURL string, port int, apiKey string) {
	e.Type = engineType
	e.BaseURL = baseURL
	if name != "" {
		e.Name = name
	}
	if apiKey != "" {
		e.APIKey = apiKey
	}
	e.Port = 0 // legacy field; both engine types carry the port in BaseURL
}

func handleRemoveEngine(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/settings-page") {
		return
	}

	editIDStr := r.FormValue("engine_id")
	if editIDStr == "" {
		addFlash(w, r, "error", "Engine ID required")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	editID, err := strconv.Atoi(editIDStr)
	if err != nil {
		addFlash(w, r, "error", "Invalid engine ID")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	engines, err := getEngines()
	if err != nil {
		slog.Error("getEngines", "err", err)
		addFlash(w, r, "error", "Could not read the current engine list; nothing was changed")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	var filtered []EngineConfig
	for _, e := range engines {
		if e.ID != editID {
			filtered = append(filtered, e)
		}
	}

	if len(filtered) == len(engines) {
		addFlash(w, r, "error", "Engine not found")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}

	if err := saveEngines(filtered); err != nil {
		slog.Error("saveEngines", "err", err)
		addFlash(w, r, "error", "Failed to remove engine")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	slog.Info("Engine removed", "id", editID)
	addFlash(w, r, "success", "Engine removed")
	http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
}

// ─────────────────────────────── Model search ─────────────────────────────────

// ─────────────────────────────── Password change ──────────────────────────────

// changePasswordPage is the page the Change Admin Password form lives on. The
// form posts to /admin/change-password, but that route is the form's POST
// endpoint, not a page — no GET is registered for it, so a browser redirect
// there would land on a 405. Every redirect that must land the admin on the
// form therefore targets the settings page instead.
const changePasswordPage = "/admin/settings-page"

// requirePasswordChanged reports whether this request may serve content while
// the admin account still has the default password. While mustChangePassword()
// holds, the only thing the admin is allowed to do is change the password, so
// the caller redirects to changePasswordPage and stops.
//
// Deliberately exempt, and why:
//   - login and logout never call it (they must stay reachable — logout is the
//     escape hatch while locked);
//   - the settings page does not call it: it hosts the form, and it is the
//     redirect target above. Guarding it would loop the redirect onto itself
//     and leave the flag set forever, since only handleChangePassword clears
//     it, and that is only reachable from the form.
//
// The isAdminSession check is redundant with the callers' own check, which
// runs first — it exists so a future caller cannot drop that check and start
// bouncing anonymous visitors at the settings page.
func requirePasswordChanged(w http.ResponseWriter, r *http.Request) bool {
	if !isAdminSession(r) || !mustChangePassword() {
		return true
	}
	http.Redirect(w, r, changePasswordPage, http.StatusFound)
	return false
}

func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/settings-page") {
		return
	}
	current := r.FormValue("current")
	newPw := r.FormValue("new")
	confirm := r.FormValue("confirm")

	admin, err := getAdmin()
	if err != nil || admin == nil || !verifyPassword(admin.PasswordHash, current) {
		addFlash(w, r, "error", "Invalid current password")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	if newPw != confirm {
		addFlash(w, r, "error", "Passwords do not match")
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	valid, msg := validatePassword(newPw)
	if !valid {
		addFlash(w, r, "error", msg)
		http.Redirect(w, r, "/admin/settings-page", http.StatusFound)
		return
	}
	if err := updateAdminPassword(admin.ID, hashPassword(newPw)); err != nil {
		slog.Error("change password", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	// Clear the default-password flag the login handler may have set: the new
	// hash is stored, and validatePassword above rejects the weak shape
	// ("admin123" has no uppercase or digit), so no future login can verify
	// against the default and re-set it. Without this, the requirePasswordChanged
	// guard would keep bouncing the admin to this page after they already won.
	SetMustChangePassword(false)
	slog.Info("Admin password changed")
	// Invalidate session so the admin must re-authenticate with the new password.
	sess := getSession(r)
	sess.Values["admin"] = false
	b, _ := json.Marshal([]FlashMsg{{"success", "Password updated. Please log in again."}})
	sess.Values["flashes"] = string(b)
	// The password row is already updated and cannot be rolled back. A 500 here
	// would tell the admin the change failed when it succeeded, sending them
	// back to a password that no longer works — the worse of the two outcomes.
	//
	// The store is a CookieStore, so a failed save does leave this browser
	// holding admin=true until the cookie expires. That is the same principal
	// who just authenticated to change their own password, so the cost is a
	// skipped re-login, not an escalation.
	_ = saveSession(w, r, sess)
	http.Redirect(w, r, "/", http.StatusFound)
}

// ─────────────────────────────── User management ──────────────────────────────

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !parseAdminForm(w, r, "/admin/panel") {
		return
	}
	name := truncateInput(r.FormValue("name"))

	if !validateName(name) {
		addFlash(w, r, "error", "Client name must be between 1 and 100 characters")
		http.Redirect(w, r, "/admin/panel", http.StatusFound)
		return
	}

	apiKey := generateAPIKey()
	if err := createUser(name, apiKey, cfg.DefaultRL); err != nil {
		addFlash(w, r, "error", err.Error())
		http.Redirect(w, r, "/admin/panel", http.StatusFound)
		return
	}

	slog.Info("User registered", "name", name)
	writeAPIKeyPage(w, "Registration Successful", name, apiKey)
}

// writeAPIKeyPage shows a freshly issued API key, once, in the body of the
// response that created it.
//
// The key must not travel any other way. A flash would put it in the session
// cookie, which the browser keeps re-sending until the flash is next rendered
// — encrypted, but still a copy stored outside this response; the database
// only ever holds its hash. No
// caching, for the same reason: this page is the key's only plaintext copy.
func writeAPIKeyPage(w http.ResponseWriter, heading, name, apiKey string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w,
		`<h2 style='font-family:sans-serif'>%s</h2>`+
			`<p>API key for <strong>%s</strong>:</p>`+
			`<pre style='background:#111;color:#0f0;padding:12px'>%s</pre>`+
			`<p>Save this key — it will not be shown again.</p>`+
			`<p><a href='/admin/panel'>Back to admin panel</a></p>`,
		html.EscapeString(heading), html.EscapeString(name), html.EscapeString(apiKey),
	)
}

func handleToggle(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	user, err := getUserByID(uid)
	if err != nil || user == nil {
		http.Error(w, "User not found", 404)
		return
	}
	newStatus := "active"
	if user.Status == "active" {
		newStatus = "disabled"
	}
	if err := updateUserStatus(uid, newStatus); err != nil {
		slog.Error("toggle user", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	slog.Info("User status toggled", "uid", uid, "status", newStatus)
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// handleAdminResetKey rotates a user's API key. The form's grace_hours field
// chooses how long the old key stays valid: 0 (the default) revokes it at
// once — exactly the pre-grace behavior — while 1, 6, or 24 gives a client
// mid-deployment time to pick up the new key. RotateAPIKey handles the grace
// window internally (the old hash lands in api_key_rotations with an expiry);
// this handler only picks the window and tells the admin about it.
func handleAdminResetKey(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	user, err := getUserByID(uid)
	if err != nil || user == nil {
		http.Error(w, "User not found", 404)
		return
	}
	if !parseAdminForm(w, r, "/admin/panel") {
		return
	}
	// Blank means "not sent" (an old tab, or the field stripped), and is
	// treated as 0 rather than an error: immediate revocation is the legacy
	// behavior, so a request that predates the selector keeps doing it.
	// Values off the form's list are rejected, not clamped — silently
	// shrinking a 48h request to 24h would leave the admin believing a
	// retired key lives longer than it does, and rounding a negative up to
	// 0 would turn a typo into an immediate revocation.
	graceHours, ok := parseGraceHours(r.FormValue("grace_hours"))
	if !ok {
		addFlash(w, r, "error", "Grace period must be 0 (immediate), 1, 6, or 24 hours")
		http.Redirect(w, r, "/admin/panel", http.StatusFound)
		return
	}
	newKey, err := RotateAPIKey(uid, graceHours)
	if err != nil {
		slog.Error("rotate api key", "uid", uid, "grace_hours", graceHours, "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	slog.Info("API key rotated", "uid", uid, "grace_hours", graceHours)
	if graceHours > 0 {
		// The key page itself cannot carry this: it is the key's one-time
		// plaintext and renders no flashes, so the note goes into the flash
		// queue instead and surfaces on the panel the admin returns to. The
		// key never does — a flash rides the session cookie until rendered,
		// and the database holds only hashes.
		expiresAt := time.Now().UTC().Add(time.Duration(graceHours) * time.Hour)
		addFlash(w, r, "success", fmt.Sprintf(
			"Key rotated for %s. The old key stays valid until %s UTC and can be revoked early.",
			user.Name, expiresAt.Format("2006-01-02 15:04")))
	}
	writeAPIKeyPage(w, "API Key Reset", user.Name, newKey)
}

// parseGraceHours validates the rotation form's grace_hours field against the
// fixed set the selector offers. 0 is a first-class value (immediate
// revocation), not just the empty-field default: either way it means the old
// key is dead the moment this handler returns.
func parseGraceHours(raw string) (graceHours int, ok bool) {
	if raw == "" {
		return 0, true
	}
	graceHours, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	switch graceHours {
	case 0, 1, 6, 24:
		return graceHours, true
	default:
		return 0, false
	}
}

func handleUpdateRateLimit(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	if !parseAdminForm(w, r, "/admin/panel") {
		return
	}
	rl, err := strconv.Atoi(r.FormValue("rate_limit"))
	if err != nil || rl < 1 {
		rl = 1
	}
	if rl > 1000 {
		rl = 1000
	}
	if err := updateUserRateLimit(uid, rl); err != nil {
		slog.Error("update rate limit", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	slog.Info("Rate limit updated", "uid", uid, "limit", rl)
	addFlash(w, r, "success", fmt.Sprintf("Rate limit updated for user %d: %d requests/minute", uid, rl))
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// handleSetQuota sets a user's daily token quota (0 = unlimited). It mirrors
// handleUpdateRateLimit: the same per-user POST shape, the same
// flash-and-redirect on failure. Where rate limiting bounds speed
// (requests/minute), quota bounds total consumption (tokens/day) — see the
// header comment in quota.go for the split between the two.
//
// SetQuota keys the column by user *name* because that is what the request
// path and the usage aggregates key by; the uid in the route exists only so
// the form target matches the other per-user actions.
//
// Registered in setupRouter (main.go) as /admin/set-quota/{uid:[0-9]+} (POST).
func handleSetQuota(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	if !parseAdminForm(w, r, "/admin/panel") {
		return
	}
	user, err := getUserByID(uid)
	if err != nil || user == nil {
		http.Error(w, "User not found", 404)
		return
	}
	// The input on the page is deliberately blank rather than prefilled with
	// the current quota: DashboardData carries no per-user quota (the User
	// struct does not have one, and main.go owns the struct), and blank is
	// the honest "unlimited" state. An explicit 0 means the same thing.
	raw := strings.TrimSpace(r.FormValue("quota_tokens"))
	if raw == "" {
		raw = "0"
	}
	quota, err := strconv.Atoi(raw)
	if err != nil || quota < 0 {
		// A negative budget is nonsense (SetQuota rejects it too), and a
		// non-integer one is a typo that clamping would launder into a real
		// budget the admin never meant.
		addFlash(w, r, "error", "Token quota must be a whole number of tokens; 0 means unlimited")
		http.Redirect(w, r, "/admin/panel", http.StatusFound)
		return
	}
	if err := SetQuota(user.Name, quota); err != nil {
		slog.Error("set quota", "uid", uid, "user", user.Name, "err", err)
		addFlash(w, r, "error", "Could not save the token quota — nothing was changed")
		http.Redirect(w, r, "/admin/panel", http.StatusFound)
		return
	}
	slog.Info("Token quota set", "uid", uid, "user", user.Name, "quota", quota)
	if quota == 0 {
		addFlash(w, r, "success", fmt.Sprintf("Token quota for %s cleared: unlimited.", user.Name))
	} else {
		addFlash(w, r, "success", fmt.Sprintf("Token quota for %s set to %d tokens/day.", user.Name, quota))
	}
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// ─────────────────────────────── Help ─────────────────────────────────────────

func handleAdminHelp(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !requirePasswordChanged(w, r) {
		return
	}
	renderPage(w, "help", HelpData{BaseData: newBaseData(w, r)})
}

// ─────────────────────────────── Logs ─────────────────────────────────────────

func handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !requirePasswordChanged(w, r) {
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	renderPage(w, "logs", LogsData{
		BaseData: newBaseData(w, r),
		Search:   search,
	})
}

func handleAdminLogsData(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		jsonResponse(w, 401, map[string]string{"error": "Unauthorized"})
		return
	}
	const pageSize = 10
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	page := maxInt(1, queryInt(r, "page", 1))

	total, err := getRequestLogsCount(search)
	if err != nil {
		slog.Error("getRequestLogsCount", "err", err)
		jsonResponse(w, 500, map[string]string{"error": "Internal server error"})
		return
	}

	pages := maxInt(1, (total+pageSize-1)/pageSize)
	page = minInt(page, pages)
	offset := (page - 1) * pageSize

	slice, err := getRequestLogsPage(search, pageSize, offset)
	if err != nil {
		slog.Error("getRequestLogsPage", "err", err)
		jsonResponse(w, 500, map[string]string{"error": "Internal server error"})
		return
	}

	jsonResponse(w, 200, map[string]interface{}{
		"logs":   slice,
		"count":  len(slice),
		"total":  total,
		"page":   page,
		"pages":  pages,
		"search": search,
	})
}

func handleAdminSystemLogs(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		jsonResponse(w, 401, map[string]string{"error": "Unauthorized"})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	page := maxInt(1, queryInt(r, "page", 1))

	entries := syslogBuf.Entries()
	// Reverse (newest first)
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if q != "" {
		var filtered []SysLogEntry
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Msg), q) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	total := len(entries)
	pages := maxInt(1, (total+9)/10)
	page = minInt(page, pages)
	start := (page - 1) * 10
	end := minInt(start+10, total)
	var slice []SysLogEntry
	if start < total {
		slice = entries[start:end]
	}

	jsonResponse(w, 200, map[string]interface{}{
		"logs":   slice,
		"count":  len(slice),
		"total":  total,
		"page":   page,
		"pages":  pages,
		"search": q,
	})
}

func handleAdminSystemLogsExport(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	entries := syslogBuf.Entries()
	// Reverse
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if q != "" {
		var filtered []SysLogEntry
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Msg), q) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=system_logs.csv")
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	cw.Write([]string{"timestamp", "level", "logger", "message"})
	for _, e := range entries {
		cw.Write([]string{e.TS, e.Level, e.Name, e.Msg})
	}
	cw.Flush()
}

func handleAdminSystemLogsReset(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		jsonResponse(w, 401, map[string]string{"error": "Unauthorized"})
		return
	}
	syslogBuf.Clear()
	slog.Info("System log buffer cleared by admin")
	jsonResponse(w, 200, map[string]bool{"ok": true})
}

// ─────────────────────────────── CSV Export ───────────────────────────────────

func handleExport(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	rows, err := getUserLogs(uid)
	if err != nil {
		slog.Error("getUserLogs", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=user_%d_audit.csv", uid))
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	cw.Write([]string{"log_id", "prompt", "response", "timestamp"})
	for _, row := range rows {
		cw.Write([]string{strconv.Itoa(row.ID), row.Prompt, row.Response, row.TS})
	}
	cw.Flush()

	// Mark as exported in session
	sess := getSession(r)
	sess.Values[fmt.Sprintf("exported_%d", uid)] = true
	// The CSV is already written and flushed to the client. A lost flag only
	// means the UI will offer the export again.
	_ = saveSession(w, r, sess)
	slog.Info("CSV export", "uid", uid)
}

func handleExportAllLogs(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	sqlRows, err := dbRead.QueryContext(r.Context(),
		`SELECT `+requestLogColumns+` FROM request_logs ORDER BY ts DESC`)
	if err != nil {
		slog.Error("request_logs export", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	defer sqlRows.Close()

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=request_logs.csv")
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	cw.Write([]string{"id", "timestamp", "method", "path", "client_ip", "client_name", "status_code",
		"request_id", "model_requested", "model_served", "engine_name", "engine_type",
		"streamed", "ttft_ms", "duration_ms", "in_tokens", "out_tokens",
		"stop_reason", "outcome", "upstream_error", "user_agent"})
	for sqlRows.Next() {
		var row RequestLogRow
		if err := sqlRows.Scan(&row.ID, &row.TS, &row.Method, &row.Path, &row.ClientIP, &row.UserName, &row.StatusCode,
			&row.RequestID, &row.ModelRequested, &row.ModelServed, &row.EngineName, &row.EngineType,
			&row.Streamed, &row.TTFTMs, &row.DurationMs, &row.InTokens, &row.OutTokens,
			&row.StopReason, &row.Outcome, &row.UpstreamError, &row.UserAgent); err != nil {
			// The 200 and the header row are already on the wire, so there is no
			// error status left to send. Stop at the first bad row: a truncated
			// prefix is recoverable by the operator, a silently skipped row in
			// the middle of an audit export is not.
			slog.Error("request_logs export scan", "err", err)
			break
		}
		cw.Write([]string{
			strconv.Itoa(row.ID), row.TS, row.Method, row.Path,
			row.ClientIP, row.UserName, strconv.Itoa(row.StatusCode),
			row.RequestID, row.ModelRequested, row.ModelServed, row.EngineName, row.EngineType,
			strconv.Itoa(row.Streamed), strconv.Itoa(row.TTFTMs), strconv.Itoa(row.DurationMs),
			strconv.Itoa(row.InTokens), strconv.Itoa(row.OutTokens),
			row.StopReason, row.Outcome, row.UpstreamError, row.UserAgent,
		})
	}
	cw.Flush()
	if err := sqlRows.Err(); err != nil {
		slog.Error("request_logs export iteration", "err", err)
	}
	slog.Info("CSV export request logs")
}

// ─────────────────────────────── Delete user ──────────────────────────────────

func handleConfirmDelete(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	renderPage(w, "confirm_delete", ConfirmDeleteData{
		BaseData: newBaseData(w, r),
		UID:      uid,
	})
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid := uidFromVars(r)
	sess := getSession(r)

	exported, _ := sess.Values[fmt.Sprintf("exported_%d", uid)].(bool)
	if !exported {
		http.Error(w, "Export required before delete", http.StatusForbidden)
		return
	}

	if err := deleteUser(uid); err != nil {
		slog.Error("deleteUser", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	delete(sess.Values, fmt.Sprintf("exported_%d", uid))
	// The user row is already gone; the stale exported_ flag is cosmetic.
	_ = saveSession(w, r, sess)
	slog.Info("User deleted", "uid", uid)
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// ─────────────────────────────── Reset system ─────────────────────────────────

func handleResetSystem(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := resetSystem(); err != nil {
		slog.Error("resetSystem", "err", err)
		http.Error(w, "Internal server error", 500)
		return
	}
	slog.Info("System reset: all logs and rate-limit entries cleared")
	addFlash(w, r, "success", "System reset: all logs and rate-limit entries have been cleared.")
	// The reset already happened. This parse only feeds the redirect-target
	// read below, which is guarded by an allow-list, so a malformed body
	// falls through to the default target — the safe answer either way.
	_ = r.ParseForm()
	// Allow-list, not a free-form path: "next" is attacker-controllable, and
	// echoing it into a redirect unchecked is an open-redirect.
	dest := "/admin/panel"
	switch r.FormValue("next") {
	case "admin_logs":
		dest = "/admin/logs"
	case "settings_page":
		dest = "/admin/settings-page"
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// ─────────────────────────────── Users redirect ───────────────────────────────

func handleUsers(w http.ResponseWriter, r *http.Request) {
	if !isAdminSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/panel", http.StatusFound)
}

// ─────────────────────────────── Helpers ──────────────────────────────────────

// parseAdminForm parses a browser form post, reporting failure the way every
// other validation failure in these handlers does: a flash plus a redirect back
// to the page that submitted it. Returns false when the caller should stop.
//
// The alternative — ignoring the error — is worse than it looks: a malformed
// body does not stop the handler, it reaches it as silently-empty form values.
// handleUpdateRateLimit, for one, would then read "" as the limit, fail the
// Atoi, and clamp the user to 1 request per window without saying why.
func parseAdminForm(w http.ResponseWriter, r *http.Request, back string) bool {
	if err := r.ParseForm(); err != nil {
		slog.Warn("malformed form submission", "path", r.URL.Path, "err", err)
		addFlash(w, r, "error", "Could not read the submitted form. Please try again.")
		http.Redirect(w, r, back, http.StatusFound)
		return false
	}
	return true
}

// saveSession persists a session and reports whether it stuck.
//
// The store writes through Set-Cookie, so a failure means the browser keeps the
// cookie it already had. Callers that are about to *announce* a state change —
// logged in, logged out — must not redirect on false: the redirect would assert
// a session the client never received. Callers whose change has already landed
// elsewhere (a password row, a deleted user) should carry on; the log line is
// the whole remedy available to them.
func saveSession(w http.ResponseWriter, r *http.Request, sess *sessions.Session) bool {
	if err := sess.Save(r, w); err != nil {
		slog.Error("session save failed", "path", r.URL.Path, "err", err)
		return false
	}
	return true
}

func uidFromVars(r *http.Request) int {
	vars := mux.Vars(r)
	uid, _ := strconv.Atoi(vars["uid"])
	return uid
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func jsonResponse(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already on the wire, so this cannot become an error
		// response. It is still worth a line, because unlike a plain Write this
		// also fails on values that cannot be marshalled at all — a bug in the
		// caller rather than a client that hung up.
		slog.Warn("json response encode", "status", status, "err", err)
	}
}

// handleSetQuota is registered in setupRouter (main.go) as
// /admin/set-quota/{uid:[0-9]+} (POST), alongside the other per-user actions.
