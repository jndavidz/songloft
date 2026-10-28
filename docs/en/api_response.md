# API Response Format Specification

This project adopts a **RESTful direct-return style** and does not use a unified `{code, data, message}` envelope.

## Permanent access tokens (Home Assistant / ESPHome)

Open token management in client settings (Flutter: Settings → Account → Permanent access tokens; Lynx: Settings → Permanent access tokens), enter a device name, and create a token. Copy the token or the complete `Authorization: Bearer <token>` header. The complete credential is shown only once and cannot be recovered after closing. If copying fails, select and copy it manually in the current dialog.

- Permanent tokens have no expiry and need no refresh. They survive server restarts, Docker updates and client logout. Recreate them if the database is lost or restored to a backup from before their creation.
- The initial version grants administrator access to business APIs, including plugin controls and resources. Token management requires a regular signed-in Access Token; permanent tokens, Refresh Tokens and plugin system tokens receive `403` there.
- Use a separate token per device. Revocation immediately makes subsequent authentication return `401`; requests already in progress are not interrupted. Other tokens remain valid.
- The database stores only a SHA-256 credential hash. Lists expose neither the complete credential nor its hash. Last-use time is updated at most once per minute and is `null` until first use.

| Endpoint | Request / response |
| --- | --- |
| `POST /api/v1/auth/personal-tokens` | `{"name":"Home Assistant"}` → `201`, metadata plus a one-time `token` |
| `GET /api/v1/auth/personal-tokens?limit=20&offset=0` | `200`, `{"tokens":[...],"total":1,"limit":20,"offset":0}`, including revoked entries |
| `DELETE /api/v1/auth/personal-tokens/{id}` | No body; success and repeated revocation return `204`, unknown IDs return `404` |

Devices send `Authorization: Bearer sl_pat_...`. Image/audio clients that cannot set headers can use `?access_token=sl_pat_...`, just like ordinary access tokens. The management ID and full credential are separate values.

Creation and list responses use `Cache-Control: no-store`. Names must contain 1–80 characters after trimming, with no control characters; invalid requests return `400`. Ordinary sign-in retains its existing 7-day Access Token and 30-day Refresh Token behavior.

---

## Success Responses

### Single Entity — Return the Model Directly

```json
// GET /api/v1/songs/1
{"id":1, "title":"Sample Track", "artist":"Sample Artist", ...}
```

### Paginated List — Collection Name + Pagination Metadata

```json
// GET /api/v1/songs?limit=20&offset=0
{"songs":[...], "total":100, "limit":20, "offset":0}
```

### Operation Result — `{"message": "..."}`

```json
// DELETE /api/v1/songs/1
{"message": "Song deleted"}
```

You may use either `models.SuccessResponse` or `map[string]string{"message": "..."}`.

---

## Error Responses

All errors are returned uniformly via `respondError`, with a fixed format:

```json
{"error": "Human-readable error message", "detail": "Optional technical details"}
```

- `error`: Always present; a short, user-facing description
- `detail`: Optional; only emitted when a non-nil `err` is passed in, containing the underlying error message

Corresponding struct: `models.ErrorResponse`.

The **middleware layer** must likewise return errors in JSON format (using the local `respondAuthError`); returning plain text via `http.Error()` is **prohibited**.

### Exception: Binary Stream Endpoints

For binary stream endpoints such as playback (`/songs/{id}/play`), proxying (`/proxy`), and static files, errors may use `http.Error()` / `http.NotFound()`, since the client does not expect a JSON body.

---

## Prohibitions

| Prohibited | Reason |
|------|------|
| `{code, data, message}` envelope | `code` semantically duplicates the HTTP status code, adding parsing burden on the client |
| Custom error field names | You must use `error` + `detail`; substitutes such as `message`, `msg`, or `reason` are not allowed |
| Returning plain-text errors from API endpoints | The frontend's `response.json()` parsing will throw (except for binary stream endpoints) |

---

## Implementation Notes

| Scenario | Usage |
|------|----------|
| Return data | `respondJSON(w, status, data)` — `data` is serialized directly as top-level JSON |
| Return an error | `respondError(w, status, message, err)` — automatically builds `{error, detail}` |
| Middleware error | `respondAuthError(w, status, message, err)` — same format as `respondError` |
| jsplugin error | `writePluginUnavailable` — also uses the `{error, detail}` fields, where `detail` contains a semantic error code (e.g. `plugin_disabled` / `plugin_not_found` / `plugin_unavailable`) rather than a low-level technical error message |
