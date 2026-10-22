# MoePay Backend

Go modular monolith: Gin, PostgreSQL (pgx/pgxpool), Redis, and WebSockets.

## Run locally (macOS, no Docker)

```bash
brew install postgresql@17 redis
brew services start postgresql@17 && brew services start redis
make db                    # creates moepay and moepay_test
cp .env.example .env       # optional, the defaults work for local dev
make migrate               # go run ./cmd/migrate up
make run                   # go run ./cmd/api  -> http://localhost:8080
```

In development, `SMS_PROVIDER=log` prints the OTP to the server log (look for `DEV ONLY: OTP`).

## Layout

```
cmd/api              HTTP + WebSocket server (graceful shutdown)
cmd/migrate          goose migrations: up | down | status | version | redo
migrations/          versioned SQL, embedded in the binaries
internal/app         composition root: wiring, routes, background jobs
internal/config      typed env config + validation (strict in production)
internal/apperr      typed errors -> stable codes + HTTP status
internal/platform    postgres (pool, WithTx, migrations), redisx, logging (slog), httpx (envelope, middleware)
internal/auth        OTP challenges, sessions, JWT access tokens, refresh rotation, auth middleware
internal/user        users + profiles repository, GET /users/me
internal/onboarding  resumable onboarding state machine
internal/username    normalization, Bloom filter (memory or Redis), availability checker
internal/ratelimit   Redis fixed-window limiter
internal/realtime    WebSocket hub, tickets, Redis pub/sub fan-out
```

Handlers parse and render, services own business rules and transactions, repositories are
stateless and take a `postgres.DBTX` so the caller controls the transaction.

## Database

| Table | Purpose |
|---|---|
| `users` | phone (E.164, unique), status, username + `username_normalized` (partial unique index), KYC status |
| `profiles` | first/last name, email, date of birth, avatar (1:1 with users) |
| `onboarding_progress` | status, current step, completed steps, completed_at, version |
| `otp_challenges` | HMAC-hashed OTP, attempts, expiry, consumption |
| `sessions` | one per device: SHA-256 of current and previous refresh token, expiry, revocation |

CHECK constraints mirror the application validation, so bad data cannot get in through any path.

## API

All responses use `{status_code, success, message, data, error?: {code, details}}`.

| Method | Path | Auth | |
|---|---|---|---|
| POST | `/api/v1/auth/send-otp` | – | `{phone_number}` |
| POST | `/api/v1/auth/verify-otp` | – | `{phone_number, otp}` → tokens, user, `onboarding.next_step`, `is_user_new` |
| POST | `/api/v1/auth/refresh` | – | `{refresh_token}` → new pair (rotated) |
| POST | `/api/v1/auth/logout` | Bearer | revoke current session |
| POST | `/api/v1/auth/logout-all` | Bearer | revoke all sessions |
| GET | `/api/v1/users/me` | Bearer | |
| GET | `/api/v1/onboarding` | Bearer | saved data + `next_step` (resume point) |
| PUT | `/api/v1/onboarding/profile` | Bearer | `{first_name, last_name, email?, date_of_birth?}` |
| PUT | `/api/v1/onboarding/username` | Bearer | `{username}` |
| POST | `/api/v1/onboarding/complete` | Bearer | validates every step, idempotent |
| GET | `/api/v1/usernames/availability?username=` | Bearer | `{available, reason}` |
| POST | `/api/v1/realtime/ticket` | Bearer | single-use WebSocket ticket (30s) |
| GET | `/api/v1/ws` | Bearer header or `?ticket=` | WebSocket |
| GET | `/healthz`, `/readyz` | – | liveness / readiness |

## Auth

- **OTP**: a 6-digit code from `crypto/rand`, stored as an HMAC-SHA256 keyed by `OTP_SECRET` and bound to the phone number. It expires after 5 minutes and allows 5 attempts per code. Verification locks the challenge row (`FOR UPDATE`), so one code yields exactly one session.
- **Abuse limits**: Redis enforces the resend cooldown plus per-phone and per-IP hourly limits. PostgreSQL enforces the cooldown and hourly cap again, so the guard still holds if Redis is down.
- **Verify**: the OTP check, user get-or-create (`ON CONFLICT` on phone), onboarding init, and session creation all run in one transaction.
- **Tokens**: the access token is an HS256 JWT (15 min) with `iss`, `aud`, `sub` and the session id `sid`. The refresh token is an opaque 256-bit value stored only as SHA-256 and rotated on every refresh. Replaying an already-rotated token revokes the whole session (refresh-token reuse detection).
- **Middleware**: verifies the JWT, then checks a Redis revocation marker for `sid`. If Redis is unavailable it falls back to PostgreSQL.

## Onboarding

Steps are `profile` → `username` → `review` → `completed`. The next step is always derived from the saved data, so reinstalling the app or switching devices resumes in the right place. Each update locks the progress row, enforces ordering (`ONBOARDING_STEP_OUT_OF_ORDER`), and saves the data and step state in one transaction. `POST /onboarding/complete` checks that every required step is satisfied. Once onboarding is complete, the onboarding endpoints return `ONBOARDING_ALREADY_COMPLETED`.

## Username availability (Bloom filter)

- A Bloom filter (1M names at a 1% false-positive rate is about 1.2 MB) sits in front of PostgreSQL:
  - **Not in the filter** means definitely unused when the filter was built, so the check returns `available` without a query.
  - **Maybe in the filter** is always confirmed in PostgreSQL. A false positive never rejects a free name.
- **Initialization**: the filter is built at startup from `users` (keyset pagination). Until that finishes, checks go straight to the database.
- **Updates**: names are added after the claiming transaction commits. A rebuild every `USERNAME_BLOOM_REBUILD_INTERVAL` clears stale bits; names claimed during a rebuild are replayed into it.
- **Backends**:
  - `memory`: one filter per instance.
  - `redis`: a shared bitmap, rebuilt into a temp key and swapped in with `RENAME` under a lock.
- **Final guard**: the partial unique index on `username_normalized` arbitrates concurrent claims and returns `USERNAME_TAKEN`.

## WebSockets

- **Auth**: native clients send `Authorization: Bearer`. Browsers use a single-use Redis ticket, so tokens never appear in URLs.
- **Origins**: browser origins must be listed in `WS_ALLOWED_ORIGINS`.
- **Limits**: read size limit, ping/pong deadlines, max connections per user, bounded send buffer (slow clients are disconnected), and max connection lifetime.
- **Messages**: client messages are whitelisted (`ping`). The server emits `connected`, `onboarding.updated`, and `onboarding.completed`.
- **Delivery**: events go through Redis pub/sub, so they reach a user's devices on any instance.
- **Logout** closes the session's sockets with code `4001`.

## Open decisions / blockers

- **SMS provider**: only the dev `log` sender exists. Implement `auth.SMSSender` (MSG91, Twilio, …) once a provider and credentials are chosen. Production config refuses `log`.
- **Profile fields and reserved usernames** are minimal defaults (`internal/onboarding/service.go`, `internal/username/normalize.go`). Adjust them to product requirements.
- **Profile edits after onboarding**: no endpoint exists yet.
