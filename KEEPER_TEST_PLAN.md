# Test plan: Keeper secrets backend for Bruin

Branch: `feat/keeper-secrets-backend` (commit `cb7ae1c52`)

## 1. Purpose and scope

Verify that `--secrets-backend keeper` resolves Bruin connections from Keeper Secrets Manager correctly, fails clearly when configuration or records are wrong, and does not change behavior for any other backend.

In scope: configuration loading, record lookup by title, notes JSON parsing, caching, error messages, CLI wiring, docs accuracy.
Out of scope: Keeper SDK internals, Keeper write operations (Bruin never writes), non-Keeper backends beyond a regression check.

## 2. What is already covered by automated tests

| Area | Where | Status |
| --- | --- | --- |
| Config parsing (JSON, base64, whitespace, missing keys, invalid input) | `pkg/secrets/keeper_test.go` | Pass |
| Env var handling (neither, both, inline, file, missing file) | `pkg/secrets/keeper_test.go` | Pass |
| Record store (single fetch, not found, duplicate titles, retry after failed fetch) | `pkg/secrets/keeper_test.go` | Pass |
| Notes validation (empty, not JSON, missing `type` or `details`) and connection caching | `pkg/secrets/keeper_test.go` | Pass |
| `cmd` wiring (`keeper` case, init failure, init success) | `cmd/secrets_backend_test.go` | Pass |

All of these use mocks. Nothing so far has talked to a real Keeper service. The sections below close that gap.

## 3. Prerequisites

- A Keeper account with Secrets Manager enabled and a role allowed to create applications. Use a non-production vault or a dedicated test folder.
- The Keeper Secrets Manager CLI (`ksm`).
- A Bruin binary built from the branch: `make build` (needs Go, Rust and the ingestr hash manifest; see CLAUDE.md). Binary lands in `bin/bruin`.
- A scratch pipeline directory outside the repo. DuckDB is used so no external database is needed.

### Test data in Keeper

Create a shared folder `bruin-keeper-test` and these records. Titles are case-sensitive.

| Record title | Notes |
| --- | --- |
| `kt-duck` | `{"type":"duckdb","details":{"path":"/tmp/keeper-test.db"}}` |
| `kt-bad-json` | `{not json` |
| `kt-no-details` | `{"type":"duckdb"}` |
| `kt-empty-notes` | (leave notes empty) |
| `kt-dup` | `{"type":"duckdb","details":{"path":"/tmp/a.db"}}` |
| `kt-dup` (second record, same title) | `{"type":"duckdb","details":{"path":"/tmp/b.db"}}` |
| `kt-unshared` | valid duckdb JSON, but placed in a folder that is **not** shared with the application |

Create application `bruin-keeper-test` with **Read Only** access to `bruin-keeper-test`, redeem the one-time token with `ksm profile init`, confirm with `ksm secret list`, then export:

```bash
ksm profile export --plain --file-format json > /tmp/keeper-config.json
export KEEPER_B64="$(base64 -w0 /tmp/keeper-config.json)"
```

### Scratch pipeline

`pipeline.yml`:

```yaml
name: keeper-test
default_connections:
  duckdb: "kt-duck"
```

`assets/hello.sql`:

```sql
/* @bruin
name: main.hello
type: duckdb.sql
materialization:
  type: table
@bruin */

SELECT 1 AS id, 'keeper' AS source
```

## 4. Test cases

Run from the directory that contains `keeper-test/`. `BRUIN` below is the built binary. Unset `BRUIN_SECRETS_BACKEND` between cases unless stated.

### A. Configuration

| ID | Steps | Expected |
| --- | --- | --- |
| A1 | No `BRUIN_KEEPER_CONFIG*` set; run `bruin run --secrets-backend keeper keeper-test` | Exit non-zero. Error contains `failed to initialize Keeper client` and `must be set`. |
| A2 | Set both `BRUIN_KEEPER_CONFIG` and `BRUIN_KEEPER_CONFIG_FILE` | Error contains `only one of`. |
| A3 | `BRUIN_KEEPER_CONFIG=$KEEPER_B64` (base64) | Run succeeds (see B1). |
| A4 | `BRUIN_KEEPER_CONFIG="$(cat /tmp/keeper-config.json)"` (raw JSON) | Run succeeds. |
| A5 | `BRUIN_KEEPER_CONFIG_FILE=/tmp/keeper-config.json` | Run succeeds. |
| A6 | `BRUIN_KEEPER_CONFIG_FILE=/nonexistent.json` | Error contains `failed to read Keeper config file`. |
| A7 | `BRUIN_KEEPER_CONFIG=garbage!!` | Error contains `JSON or a base64-encoded JSON`. |
| A8 | JSON config with `privateKey` removed | Error contains `missing the 'privateKey' key`. |
| A9 | Set `KSM_CONFIG` to a **different** valid config and also set `BRUIN_KEEPER_CONFIG` to the test one | Bruin uses `BRUIN_KEEPER_CONFIG` (SDK must not fall back to `KSM_CONFIG`). Confirm via successful run against the test folder only. |
| A10 | Config file with permissions `0600` and a config copied from another machine | Works; Bruin does not write to the file (compare checksum and mtime before and after). |

### B. Happy path against a real tenant

| ID | Steps | Expected |
| --- | --- | --- |
| B1 | `bruin run --secrets-backend keeper keeper-test` | Asset `main.hello` runs, `/tmp/keeper-test.db` is created, table `main.hello` has one row. No `.bruin.yml` credentials needed. |
| B2 | `BRUIN_SECRETS_BACKEND=keeper bruin run keeper-test` (env var instead of flag) | Same result as B1. |
| B3 | `bruin --secrets-backend keeper run keeper-test` (flag before the subcommand) | Same result as B1. |
| B4 | Re-run B1 with a `.bruin.yml` present that defines a different `kt-duck` pointing at `/tmp/other.db` | Data lands in `/tmp/keeper-test.db`, proving `.bruin.yml` connections are ignored. |
| B5 | Run a two-asset pipeline that both use `kt-duck` with `--debug` or a network capture | Keeper is contacted once for the run, not once per asset or per lookup. |
| B6 | Run a pipeline whose default connection title differs only in case (`KT-DUCK`) | Fails with `no record titled 'KT-DUCK' found` (titles are case-sensitive). |

### C. Record and notes errors

| ID | Connection name in pipeline | Expected error text |
| --- | --- | --- |
| C1 | `kt-missing` (no such record) | `no record titled 'kt-missing' found` |
| C2 | `kt-unshared` | `no record titled 'kt-unshared' found` (not shared with the app) |
| C3 | `kt-dup` | `2 records are titled 'kt-dup'; record titles must be unique` |
| C4 | `kt-empty-notes` | `has an empty notes field` |
| C5 | `kt-bad-json` | `are not valid JSON` |
| C6 | `kt-no-details` | `must contain both 'type'` |
| C7 | Record with `"type":"not_a_type"` | A clear failure from the connection manager, not a panic. |

Each failure should print an error and exit non-zero without a stack trace. The error must not echo secret values (check C5 and C7 output in particular).

### D. Keeper-side changes

| ID | Steps | Expected |
| --- | --- | --- |
| D1 | Edit the notes of `kt-duck` to a new `path`, then run again | New path is used on the next run (cache does not persist across runs). |
| D2 | Remove the folder share from the application, then run | `no record titled ... found` or `failed to fetch records from Keeper`, with non-zero exit. |
| D3 | Remove the client device (or delete the application), then run | `failed to fetch records from Keeper`, non-zero exit, no hang. |
| D4 | Application has Read Only access (as configured) | Everything above passes. Confirms Bruin does not need write access. |
| D5 | Block network access to Keeper (firewall or invalid hostname in config) | Fails with a fetch error in reasonable time, no indefinite hang. Note how long it takes. |

### E. CLI and docs

| ID | Steps | Expected |
| --- | --- | --- |
| E1 | `bruin run --help` and `bruin --help` | `--secrets-backend` usage lists `keeper`. |
| E2 | `bruin run --secrets-backend bogus keeper-test` | Error lists `vault, doppler, aws, azure, keeper`. |
| E3 | Follow `docs/secrets/keeper.md` end to end on a clean machine, verbatim | Every command works as written. Record each deviation. In particular confirm `ksm profile export --plain --file-format json` yields JSON with `clientId`, `appKey`, `privateKey`, `hostname`, and that the default base64 export also works as `BRUIN_KEEPER_CONFIG`. |
| E4 | Check the claims in the doc that were not verified at writing time | One-time token is single use; behavior of IP lock when running from another IP; Read Only access is sufficient. |
| E5 | Render the docs (`npm run docs:dev` or the repo's docs command) | Keeper appears in the Secret Providers sidebar; links in `keeper.md` and `overview.md` resolve. |

### F. Other commands that take the flag

Run each with `--secrets-backend keeper` and confirm it resolves `kt-duck` or fails with the standard error:

- `bruin validate`
- `bruin query --connection kt-duck --query "select 1"`
- `bruin run` with `--downstream` and a single-asset path
- `bruin curl` is not Keeper-relevant for DuckDB; skip unless you have an HTTP connection record

### G. Regression

| ID | Steps | Expected |
| --- | --- | --- |
| G1 | `bruin run keeper-test` with no backend flag and a normal `.bruin.yml` | Behaves exactly as before. |
| G2 | `--secrets-backend vault`, `doppler`, `aws`, `azure` with their env vars unset | Same "failed to initialize" errors as on `main`. |
| G3 | `make format` and `make test` (the CLAUDE.md gates) | Both pass on the branch. Not yet run on this branch. |
| G4 | `golangci-lint run` using the repo's `.golangci.yml` | No new findings. Not yet run on this branch. |
| G5 | `go mod tidy` leaves `go.mod` and `go.sum` unchanged | Clean. |
| G6 | Binary size and startup time compared with `main` (`bruin --version`) | No meaningful regression from the new dependency. |

### H. Security checks

| ID | Check | Expected |
| --- | --- | --- |
| H1 | Run B1 with `--debug` and grep output and logs for the config values and the record notes | Neither appears. |
| H2 | Run C5 and C7 and inspect output | Error messages do not contain secret values from notes. |
| H3 | Inspect `/tmp/keeper-config.json` and the process environment after a run | Config file is unchanged (A10). Document that `BRUIN_KEEPER_CONFIG` is visible in the process environment and CI logs must mask it. |
| H4 | Confirm the SDK does not write a `client-config.json` or similar file in the working directory or home | No stray config files created. |

## 5. Exit criteria

- All cases in A to E pass on a real Keeper tenant, or each failure has a ticket and an agreed fix.
- G1, G3, G4 and G5 pass.
- Docs deviations found in E3 and E4 are corrected in `docs/secrets/keeper.md` before the PR is opened.
- No secret material appears in any output captured in H.

## 6. Known gaps and risks to watch

- The Keeper SDK method behavior (`GetSecrets` with no UIDs returns all shared records) was confirmed from source, not against a live tenant. B1 and B5 are the first real confirmation.
- Fetching all shared records on startup scales with the number of records shared with the application. If a tenant shares thousands of records, measure B1 latency and consider limiting the shared folder to Bruin records.
- The SDK logs through its own logger. Confirm it does not print noise or sensitive details on a normal run (H1).
- Keeper regional hostnames (EU, AU, JP, CA, US_GOV) come from the exported config. Test at least one non-US tenant if you have one.
- Notes edited in the Keeper web UI may be reformatted. Test a multi-line, pretty-printed JSON note as well as a single-line one (add this as B7 if it behaves differently).

## 7. Reporting

Record for each case: ID, pass or fail, actual output (with secrets redacted), Bruin commit, `ksm` version, Keeper region. File failures against the branch before opening the upstream PR.
