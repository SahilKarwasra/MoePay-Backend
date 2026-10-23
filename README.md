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
internal/money       GBP amounts in pence and "£1,234.56" formatting
internal/ledger      double-entry postings; the only code that changes balances
internal/idempotency Idempotency-Key handling, stored in the same transaction as the money movement
internal/wallet      wallets, 6-digit PIN (argon2id, lockout, OTP reset), payment QR
internal/bank        linked bank accounts (sort code + account number), dummy bank gateway
internal/payment     recipient lookup, P2P transfers, add money, withdraw
internal/moneyrequest request money: create, pay, decline, cancel, expiry
internal/history     transaction history with keyset pagination and filters
internal/dashboard   screen aggregates for the home and wallet screens
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
| `wallets` | one per user plus the `BANK_CLEARING` system wallet; cached balance, PIN hash and lockout |
| `transactions` | one row per money movement or failed attempt: type, status, amount, wallets, reference, idempotency key |
| `ledger_entries` | append-only debit/credit pairs with the balance after each entry |
| `bank_accounts` | linked accounts: sort code, last 4 digits and an HMAC of the full number |
| `money_requests` | requester, payer, amount, status, expiry, paid transaction |
| `idempotency_keys` | stored outcome per user and key, kept for 24 hours |

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

E-wallet endpoints (all Bearer). Endpoints marked 🔑 need `Idempotency-Key` and the PIN.

| Method | Path | Screen / purpose |
|---|---|---|
| GET | `/api/v1/home` | home: balance, quick actions, recent recipients, pending requests, last 3 transactions |
| GET | `/api/v1/wallet` | wallet: balance, PIN status, limits and today's usage, actions, bank accounts, last 5 transactions |
| POST · PUT | `/api/v1/wallet/pin` | set `{pin, confirm_pin}` · change `{current_pin, new_pin}` |
| POST | `/api/v1/wallet/pin/reset/otp` · `/api/v1/wallet/pin/reset` | send OTP · reset `{otp, new_pin}` |
| GET | `/api/v1/wallet/qr?amount_pence=&note=` | "pay me" QR payload `moepay://pay?u=…` |
| GET · POST | `/api/v1/bank-accounts` | list · link `{holder_name, sort_code, account_number, make_primary?}` |
| DELETE · PUT | `/api/v1/bank-accounts/:id` · `/api/v1/bank-accounts/:id/primary` | unlink · make primary |
| GET | `/api/v1/banks/sort-code/:code` | bank name for a sort code |
| GET | `/api/v1/recipients/resolve?username=` / `phone=` / `user_id=` / `qr=` | confirm who you are paying |
| GET | `/api/v1/recipients/recent` | people you recently paid or were paid by |
| POST 🔑 | `/api/v1/transfers` | `{to: {username \| phone \| user_id}, amount_pence, note?, pin}` |
| POST 🔑 | `/api/v1/wallet/add-money` · `/api/v1/wallet/withdraw` | `{bank_account_id, amount_pence, pin}` |
| POST · GET | `/api/v1/money-requests` | request `{from: {…}, amount_pence, note?}` · list `?direction=incoming\|outgoing&status=&cursor=&limit=` |
| GET | `/api/v1/money-requests/:id` | one request with the actions available to you |
| POST 🔑 | `/api/v1/money-requests/:id/pay` | `{pin}` |
| POST | `/api/v1/money-requests/:id/decline` · `/cancel` | payer declines · requester cancels |
| GET | `/api/v1/transactions?cursor=&limit=&type=&status=&direction=&from=&to=` | full history, newest first |
| GET | `/api/v1/transactions/:id` | one transaction |

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
- **Messages**: client messages are whitelisted (`ping`). The server emits `connected`, `onboarding.updated`, `onboarding.completed`, `wallet.credited`, `wallet.debited`, `money_request.received` and `money_request.updated`.
- **Delivery**: events go through Redis pub/sub, so they reach a user's devices on any instance.
- **Logout** closes the session's sockets with code `4001`.

## E-wallet

- **Money**: GBP only, stored as integer pence. Every amount in a response is `{amount_pence, currency, display}`, e.g. `"£1,234.56"`.
- **Ledger**: every movement writes a `transactions` row and a debit/credit pair in `ledger_entries`, and updates the cached `wallets.balance_pence`, all in one database transaction.
  - A deferred trigger rejects any transaction whose entries don't sum to zero.
  - Ledger entries are append-only.
  - User wallets can't go negative (CHECK constraint).
  - Bank flows post against the `BANK_CLEARING` system wallet, so the sum of all wallets is always zero.
- **Concurrency**: both wallets are locked with `SELECT … FOR UPDATE` in id order, so parallel or opposite transfers can't overdraw or deadlock.
- **API vs WebSocket**: money moves only through REST `POST`s. WebSocket only notifies: `wallet.credited`, `wallet.debited`, `money_request.received`, `money_request.updated`.
- **Idempotency**: every money-moving `POST` needs an `Idempotency-Key` header (a UUID).
  - The key and the response are stored in the same transaction as the money movement.
  - A retry with the same key and body replays the original response (header `Idempotent-Replayed: true`).
  - The same key with a different body returns `IDEMPOTENCY_KEY_REUSED`.
  - A concurrent duplicate waits for the first request, then replays it.
  - A unique index on `(initiator, idempotency_key)` backs this up.
- **PIN**: 6 digits, stored as argon2id. Sequences, repeats and common PINs are rejected.
  - After 5 wrong attempts the PIN locks for 30 minutes. The counter lives in PostgreSQL and failed attempts are committed even though the request fails.
  - Reset needs an OTP sent to the user's phone. OTPs carry a purpose, so a login code can't reset a PIN.
- **Limits** (configurable, `WALLET_*` env vars): £0.01–£5,000 per transaction, £10,000 maximum balance, and £10,000 outgoing per calendar day in `Europe/London`.
- **Dummy bank**: settles instantly.
  - It refuses to pull from accounts ending in `0000` (insufficient funds).
  - Accounts ending in `9999` are always unavailable.
  - The posting is made in a savepoint before the bank is called. On a decline it is rolled back, and a `failed` transaction is recorded so it shows in history.
  - A real bank would need asynchronous settlement with an outbox.
- **Wallet creation**: a wallet opens when onboarding completes, in the same transaction. Users who finished onboarding earlier get one on first access.
- **Money requests**: expire after 7 days (`MONEY_REQUEST_TTL`), and at most 5 can be pending between the same pair of users.
- **History**: keyset pagination on `(created_at, id)` across both directions of a wallet, with type, status, direction and date filters. Each item has a ready-to-show `title`, `signed_display` and `counterparty`.

## Open decisions / blockers

- **SMS provider**: only the dev `log` sender exists. Implement `auth.SMSSender` (MSG91, Twilio, …) once a provider and credentials are chosen. Production config refuses `log`.
- **Profile fields and reserved usernames** are minimal defaults (`internal/onboarding/service.go`, `internal/username/normalize.go`). Adjust them to product requirements.
- **Profile edits after onboarding**: no endpoint exists yet.
- **Phone region**: `PHONE_DEFAULT_REGION` defaults to `IN`. For a UK app, set it to `GB` so local numbers like `07911 123456` parse.
- **Real bank**: only the instant dummy gateway exists (`bank.Gateway`).
