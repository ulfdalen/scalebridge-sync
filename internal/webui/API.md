# Local HTTP API contract

Server binds `127.0.0.1:<port>` (default 8723). This file is the single source of truth for
the JSON API — the static UI (`static/*.js`) and the Go handlers must both match it exactly.

## Conventions

- All responses `Content-Type: application/json` except the HTML pages and `/callback`.
- Errors: HTTP status + `{"error": "snake_code", "detail": "human-readable, optional"}`.
- CSRF: every non-GET request MUST carry header `X-ScaleBridge-Local: 1`; requests with a
  body MUST be `Content-Type: application/json`. Otherwise 403 `{"error":"csrf"}`.
- Host header must be `localhost:<port>`, `127.0.0.1:<port>` or `[::1]:<port>`, else 403.
- Timestamps are RFC3339 strings; absent/never values are `null`.

## Pages

```
GET /            dashboard (302 → /setup until setup_complete)
GET /setup       wizard    (302 → / once setup_complete)
GET /settings    settings page
GET /callback    Withings OAuth redirect target (HTML result page)
GET /static/*    embedded assets
```

## Endpoints

### GET /api/status
```json
{
  "version": "0.1.0",
  "setup_complete": true,
  "withings": {"creds_set": true, "connected": true, "reconnect_required": false, "user_id": "123456"},
  "garmin":   {"connected": true, "reconnect_required": false, "email": "user@example.com"},
  "sync": {"running": false, "last_at": "2026-08-25T10:00:00Z", "last_error": "",
           "last_fetched": 1, "last_uploaded": 1, "next_at": "2026-08-25T10:15:00Z",
           "interval_minutes": 15},
  "backfill": {"running": false, "done": 0, "total": 0},
  "update": {"newer": false, "latest": "", "url": ""},
  "config_path": "/Users/me/Library/Application Support/scalebridge-sync/state.json"
}
```
`sync.last_at`/`next_at` are `null` before the first tick / when not scheduled.
`update` is the cached result only — this handler never calls GitHub.

### GET /api/setup/state
```json
{
  "port": 8723,
  "callback_url": "http://localhost:8723/callback",
  "client_id": "abc123…",
  "client_secret_set": true,
  "steps": {"withings_creds": true, "withings_oauth": false, "garmin": false, "backfill": false},
  "last_oauth_error": "",
  "last_oauth_detail": ""
}
```
Never contains the client secret. `last_oauth_error` values: `""`, `"denied"`, `"state_mismatch"`,
`"exchange_failed"`; `last_oauth_detail` carries the human-readable text (the Withings client's
already-redacted error string). Both are in-memory wizard state, reset by a restart.

### PUT /api/setup/withings-credentials
Req `{"client_id": "…", "client_secret": "…"}` → `{"ok": true}`.
Side effect: if client_id changed, stored Withings tokens are cleared (steps.withings_oauth → false).

### GET /api/withings/connect
302 → Withings authorize2 URL. Mints a single-use `state` nonce (10-min TTL, in-memory) and
clears `last_oauth_error` (the wizard only reports the value when it *changes*, so two identical
failures in a row must not collapse into one).
409 `{"error":"no_credentials"}` if creds not set.

### GET /callback?code&state  (or ?error=…)
HTML result page ("Connected — you can close this tab" / error text). Exchanges the code,
stores tokens + user_id, clears/sets `last_oauth_error`.

### POST /api/withings/disconnect → `{"ok": true}`  (clears tokens, keeps creds)

### POST /api/garmin/login
Req `{"email": "…", "password": "…"}` →
`{"ok": true}` | `{"mfa_required": true, "mfa_method": "email", "mfa_token": "opaque"}` |
401 `{"error": "invalid_credentials"}` | 502 `{"error": "garmin_unreachable"}`.

### POST /api/garmin/verify-mfa
Req `{"mfa_token": "…", "code": "123456"}` → `{"ok": true}` |
401 `{"error": "invalid_mfa_code"}` | 410 `{"error": "mfa_expired"}` (restart login).

### POST /api/garmin/disconnect → `{"ok": true}`

### POST /api/setup/backfill
Req `{"choice": "none"|"30d"|"1y"|"all"}` → `{"ok": true}` (stores the choice; runs on setup complete).

### POST /api/setup/complete
Req `{"interval_minutes": 15}` → `{"ok": true}`. Marks setup done, starts scheduler, kicks
first sync (+ chosen backfill).

### POST /api/sync/now → `{"started": true}` | 409 `{"error": "already_running"}`
### POST /api/sync/backfill
Req `{"range": "30d"|"1y"|"all"}` → `{"started": true}` | 409 `{"error": "already_running"}`

### GET /api/measurements?limit=50
```json
{"items": [{"measured_at": "…", "weight_kg": 80.5, "body_fat_pct": 18.2, "muscle_kg": null,
            "bone_kg": null, "hydration_pct": null, "bmi": 24.1,
            "synced": true, "sync_error": ""}]}
```

### GET /api/events?limit=20
```json
{"items": [{"at": "…", "level": "info", "message": "Synced 1 measurement"}]}
```
`level`: `info` | `warn` | `error`. Newest first.

### GET /api/settings → `{"interval_minutes": 15, "port": 8723, "update_check": false}`
### PUT /api/settings
Req: any subset of the three fields → `{"ok": true, "restart_required": false}`
(`restart_required: true` when port changed; port takes effect on restart).

### GET|POST /api/update/check
→ `{"current": "0.1.0", "latest": "0.2.0", "url": "https://github.com/…", "newer": true}`
502 `{"error": "github_unreachable"}` on failure. Server-side call, cached 24h; GET serves the
cache when it is fresh, POST ("Check now") always refetches. `newer` is false for a `dev` build.
