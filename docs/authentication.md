# Authentication

Inbound requests authenticate against named bearer keys declared in the
config document's `auth` section. There is no auth environment variable —
keys live in the file, env-interpolated at load.

```yaml
auth:
  keys:
    - name: primary
      key: ${OFP_PRIMARY_API_KEY}
    - name: backup
      key: ${OFP_BACKUP_API_KEY}
```

Clients present `Authorization: Bearer <key>` on every request.

## Multiple keys

`keys[]` is a list — issue one entry per client or environment and revoke it
independently by rewriting the file. Two entries may not share a `name` and
may not share a `key` value (both are load errors; see validation below).

## What is logged — and what never is

A matched request's completion log line carries the key **name**:

```text
ab12cd34 generation=3 route=default egress=primary attempts=1 class=ok status=200 ... api_key_name="primary"
```

The name is the only part of a key that is ever echoed — not in logs, not in
load/validation errors, not in 401 responses, not in debug output. The value
is held in the runtime snapshot's private key table (`Runtime.LookupAPIKey`
returns the name only), so no caller can leak it by accident. Keep names
free of secrets and control characters; they reach log lines verbatim.

## Auth off (empty keys)

An **empty or omitted `auth` section disables authentication** — every
request is admitted. This is the built-in no-config runtime's behavior (the
historical default), and it is explicit: an empty `keys: []` list means the
same thing. There is no anonymous-key middle ground — an entry with an empty
`key` value is a load error, so a deployment can never believe auth is on
when the file actually disabled it.

## Invalid or missing key

A request whose bearer credential matches no configured key is rejected with

```json
{
  "error": {
    "message": "Invalid API key provided",
    "type": "authentication_error",
    "code": "invalid_api_key"
  }
}
```

(HTTP 401) before the method check, the body read, or anything else — the
401 path touches no lock, no health state, and no upstream. The presented
credential is never echoed back.

## Duplicate names / duplicate values

Both are rejected at load:

- duplicate `name` → `auth.keys[N]: duplicate key name "…"`;
- duplicate `key` value → `auth.keys[N] (name): duplicate key value (also
used by auth key "…")`.

The second error identifies the colliding entries **by name** — the secret
value never appears. Also rejected: empty name, empty key value, names
containing control characters.

## Environment interpolation

Key values are `${VAR}` references resolved over the raw file bytes at load
(comments included). An unset variable is a load error naming the variable —
a deployment with a typo'd env var fails loudly at startup (or on the
rejected reload), never silently auths against an empty credential.

## Reload semantics

`auth.keys` is part of the per-generation runtime snapshot, exactly like
egresses and routes:

- a key **added** by a reload admits requests that arrive after the swap;
- a key **revoked** by a reload stops admitting new requests immediately at
  the swap;
- a request **already admitted** keeps its admission (and its logged
  `api_key_name`) for its whole lifetime — one request is bound to one
  generation, auth included (see [architecture.md](architecture.md)).

The completion log's `generation=N` and `api_key_name` therefore always
describe the same generation the request was served under.
