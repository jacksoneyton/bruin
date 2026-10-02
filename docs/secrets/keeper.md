# Using Keeper as a Secrets Backend

Bruin can read connection credentials from [Keeper Secrets Manager](https://docs.keeper.io/en/keeperpam/secrets-manager/about) (KSM) instead of from `.bruin.yml`. This guide covers the setup on the Keeper side, the setup on the Bruin side, and complete examples.

## How it works

1. Keeper holds one **record per Bruin connection**. The record **title** is the connection name, and the record **notes** field holds a JSON document describing the connection.
2. A Keeper **application** is granted access to the folder containing those records. Bruin authenticates as that application using a client configuration.
3. When Bruin runs with `--secrets-backend keeper`, it loads the records shared with the application once, finds the record whose title matches each connection a pipeline needs, and builds the connection from the JSON in the notes.
4. Connections defined in `.bruin.yml` are not used while a secrets backend is active.

## Part 1: Keeper-side setup

### 1. Create a shared folder and records

1. In the Keeper Vault, create a **shared folder** for Bruin, for example `bruin-connections`. Only records in shared folders can be shared with a Secrets Manager application.
2. Create one record per connection inside that folder. Any record type works, because Bruin only reads the title and the notes.
3. Set the **Title** to the exact connection name that your pipelines use (for example `my-postgres`). Titles are case-sensitive and must be unique among the records shared with the application. Bruin fails with an error when it finds duplicates instead of choosing one.
4. Paste the connection JSON into the **Notes** field (see [Record format](#record-format)). Use plain ASCII quotes. Notes edited in word processors may be converted to curly quotes, which are not valid JSON.

### 2. Create a Secrets Manager application

You need Secrets Manager enabled on your Keeper subscription, and a role that allows creating applications.

In the Keeper Vault:

1. Open the **Secrets Manager** tab and choose **Create Application**.
2. Enter a name, for example `bruin`.
3. Select the shared folder from the previous step.
4. Choose **Read Only** access. Bruin never writes to Keeper, so do not grant edit access.
5. Optionally lock the first client device to its IP address. If you enable this, run the profile initialization in step 3 from an allowed IP.
6. Choose **Generate Access Token** and copy the **one-time access token** immediately. It cannot be shown again, and it expires if unused. If you lose it, add a new client device to the application to get a new token.

With Keeper Commander the equivalent is:

```bash
secrets-manager app create bruin
secrets-manager share add --app bruin --secret <SHARED_FOLDER_UID>
secrets-manager client add --app bruin
```

The last command prints the one-time token.

### 3. Turn the one-time token into a client configuration

Bruin does not use the one-time token directly. It uses the client configuration that the Keeper Secrets Manager CLI (`ksm`) creates from that token, because the token can be redeemed only once. Use the CLI to redeem it:

```bash
# Keep the token out of your shell history by using the environment variable
export KSM_CLI_TOKEN='US:xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'
ksm profile init
```

The token starts with your Keeper region (`US`, `EU`, `AU`, `JP`, `CA`, `US_GOV`). Check that the application can see the records:

```bash
ksm secret list
```

You should see the records from your shared folder. Then export the configuration for Bruin:

```bash
ksm profile export --plain --file-format json > keeper-config.json
```

`BRUIN_KEEPER_CONFIG` also accepts the default base64 output of `ksm profile export`, so either form works. The configuration contains the `hostname`, `clientId`, `privateKey` and `appKey` values. Treat it as a credential: anyone who has it can read every record shared with the application. Store it in your CI system's secret store, not in the repository.

## Part 2: Bruin-side setup

### Environment variables

Provide the client configuration with **one** of the following:

| Variable | Description |
| --- | --- |
| `BRUIN_KEEPER_CONFIG` | The client configuration as a JSON string or a base64-encoded JSON string. |
| `BRUIN_KEEPER_CONFIG_FILE` | The path to a file containing the client configuration JSON. |

Setting both is an error. The configuration must contain the `clientId`, `appKey` and `privateKey` keys. Bruin passes the configuration to the SDK explicitly, so the `KSM_CONFIG` variable and the SDK's default config file are not consulted.

Select the backend with the flag or the environment variable:

```bash
bruin run --secrets-backend keeper
```

```bash
export BRUIN_SECRETS_BACKEND=keeper
bruin run
```

### Record format

The notes of each record must be a JSON document with a `type` and a `details` object. This is the same format used by the Vault, Doppler, AWS and Azure backends.

Record title: `my-postgres`. Notes:

```json
{
  "type": "postgres",
  "details": {
    "host": "db.example.com",
    "port": 5432,
    "username": "bruin",
    "password": "s3cret",
    "database": "analytics",
    "schema": "public"
  }
}
```

Record title: `my-snowflake`. Notes:

```json
{
  "type": "snowflake",
  "details": {
    "account": "my-account",
    "username": "bruin",
    "password": "s3cret",
    "warehouse": "my-warehouse",
    "database": "analytics",
    "schema": "public"
  }
}
```

The `type` must be a valid Bruin connection type, and `details` takes the same fields as the matching entry in `.bruin.yml`. See the [connections documentation](../getting-started/introduction/quickstart.md#setting-up-your-bruin-yml-file) for the supported types. Bruin sets the connection `name` from the record title, so do not repeat it in `details`.

### Referencing the connections in a pipeline

The pipeline refers to connections by the same names as the record titles. For the two records above:

`pipeline.yml`

```yaml
name: analytics
schedule: daily

default_connections:
  postgres: "my-postgres"
  snowflake: "my-snowflake"
```

`assets/orders_raw.asset.yml`, an ingestion asset that reads from Postgres and writes to Snowflake:

```yaml
name: raw.orders
type: ingestr
parameters:
  source_connection: my-postgres
  source_table: public.orders
  destination: snowflake
```

`assets/daily_orders.sql`, a transformation that runs on Snowflake:

```sql
/* @bruin
name: analytics.daily_orders
type: sf.sql
materialization:
  type: table
depends:
  - raw.orders
@bruin */

SELECT order_date, COUNT(*) AS orders
FROM raw.orders
GROUP BY order_date
```

Run it:

```bash
export BRUIN_KEEPER_CONFIG="$(base64 -w0 keeper-config.json)"
bruin run --secrets-backend keeper ./analytics
```

The flag is global, so it can also appear before the subcommand, for example `bruin --secrets-backend keeper curl ...`.

### CI example

The configuration is stateless, so it works on ephemeral runners. In GitHub Actions:

```yaml
jobs:
  run:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Run pipeline
        env:
          BRUIN_SECRETS_BACKEND: keeper
          BRUIN_KEEPER_CONFIG: ${{ secrets.KEEPER_CONFIG }}
        run: bruin run ./analytics
```

Store the contents of `keeper-config.json` (or its base64 form) as the `KEEPER_CONFIG` repository secret.

## Behavior notes

- Records are fetched once per Bruin process and cached in memory, so changing a record in Keeper takes effect on the next run, not during a run.
- Only the records in folders shared with the application are visible. A connection that exists in Keeper but lives in an unshared folder is reported as not found.
- Rotating credentials means editing the record in Keeper. The application configuration only changes if you remove the client device or application.

## Troubleshooting

| Error | Cause and fix |
| --- | --- |
| `BRUIN_KEEPER_CONFIG or BRUIN_KEEPER_CONFIG_FILE env variable must be set` | No configuration was provided. |
| `only one of BRUIN_KEEPER_CONFIG and BRUIN_KEEPER_CONFIG_FILE can be set` | Both variables are set. Unset one. |
| `Keeper configuration must be JSON or a base64-encoded JSON string` | The value is neither. Re-export it with `ksm profile export --plain --file-format json`. |
| `Keeper configuration is missing the '<key>' key` | The configuration is incomplete. Re-export it from an initialized profile. |
| `failed to fetch records from Keeper` | Authentication or network failure. Confirm that `ksm secret list` works with the same configuration and that the client device was not removed. |
| `no record titled '<name>' found` | The record does not exist, the title differs (titles are case-sensitive), or its folder is not shared with the application. |
| `records are titled '<name>'; record titles must be unique` | More than one shared record has that title. Rename or unshare one. |
| `record '<name>' has an empty notes field` | Put the connection JSON in the record's Notes field. |
| `notes of record '<name>' are not valid JSON` | The notes are not valid JSON. Check for curly quotes or trailing commas. |
| `record '<name>' must contain both 'type' ... and 'details'` | Add the missing field to the JSON. |
