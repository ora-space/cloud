# cmd/ora-web: local frontend development edge

[中文](README.md) | [English](README.en.md)

Provides a local HTTP edge for `frontend/`, composing the real Cloud router, development login and collaboration fixtures. It is not a production Gateway.

## Data and entrypoints

- `ORA_DATABASE_URL` selects PostgreSQL; the default is the local `ora` database at `127.0.0.1:55432` (connection details in `main.go`).
- Creates and retains the fixed `ora_web` schema, applies embedded migrations at startup and ensures a development tenant exists. Restarts retain data.
- `POST /auth/login` selects or creates a development member from the supplied email; `POST /auth/register` creates a separate tenant; `POST /auth/logout` clears the cookie. These development endpoints do not verify external identity.
- For `/api/` requests the server signs service/user JWTs from the `ora_subject` cookie before handing them to the Cloud router. The cookie holds a development subject, not a production session credential.
- `ORA_WEB_ADDR` defaults to `127.0.0.1:8080`. `ORA_WEB_DEV=1` redirects frontend requests to `http://localhost:5173`; otherwise `frontend/dist` supplies the SPA and client-route fallback.

Run `go run ./cmd/ora-web` from the repository root; static mode needs a frontend build first. Shutdown signals close HTTP and database connections.

## Development boundary

Keys are generated in memory on each run. [collab](../../internal/collab/README.en.md) fixtures are wired unconditionally for demos. Use [gateway](../gateway/README.en.md) for production login and sessions; see [Local development](../../docs/development/local-development.en.md) for the regular setup.
