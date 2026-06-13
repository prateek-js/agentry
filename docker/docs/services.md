# Services — binding + data namespacing (EVERY project kind)

Applies to nextjs, fastapi, streamlit, python-script, custom — the
examples are TypeScript but the pattern is identical in Python.

Tell the user what they need to bind BEFORE writing data-access
code. Then write code that reads env vars; never hardcode connection
strings or API keys.

| Service     | Env var(s)                                | Node SDK                 | Python SDK            |
|-------------|-------------------------------------------|--------------------------|-----------------------|
| postgres    | `DATABASE_URL`                            | `postgres` or `drizzle`  | `psycopg` / SQLAlchemy|
| mysql       | `DATABASE_URL`                            | `mysql2`                 | `pymysql`             |
| mongodb     | `MONGODB_URL`                             | `mongodb`                | `pymongo`             |
| redis       | `REDIS_URL`                               | `ioredis`                | `redis`               |
| clickhouse  | `CLICKHOUSE_URL` etc.                     | `@clickhouse/client`     | `clickhouse-connect`  |
| aws-s3      | `AWS_ACCESS_KEY_ID` etc.                  | `@aws-sdk/client-s3`     | `boto3`               |
| smtp        | `SMTP_HOST`, `SMTP_PORT` etc.             | `nodemailer`             | `smtplib`             |
| stripe      | `STRIPE_SECRET_KEY`                       | `stripe`                 | `stripe`              |
| openai      | `OPENAI_API_KEY`                          | `openai`                 | `openai`              |
| anthropic   | `ANTHROPIC_API_KEY`                       | `@anthropic-ai/sdk`      | `anthropic`           |

Pattern, ALWAYS (CONTRACT.md rule 3):
1. `service_list` to confirm what the cluster has bindable.
2. `service_bind(sandbox_id=..., service="postgres")` — or confirm
   the binding already exists in the sandbox_create response.
3. Read env vars in your code (`process.env.DATABASE_URL!` /
   `os.environ["DATABASE_URL"]`). Never hardcode. Never inline
   secrets.
4. Start the project (`project_start`) AFTER the bind so the shell
   shim picks up the env on launch.

## Sharing one service across many apps — namespace OR clobber

The user binds ONE mongo / postgres / redis / s3 per cluster. Every
sandbox AND every deployed app inherits the SAME credentials, which
means the SAME data store. Two apps that both write to a collection
called `users` will overwrite each other.

**Rule: every app namespaces its writes under its own name. Never use
default db names, default schemas, or bare collection / key names.**

The platform stamps `AGENTRY_APP_NAME` into every app's env — the
deployment slug in prod, the sandbox id in dev. Read it; don't
override it. The fallback `?? "dev"` exists so a brand-new sandbox
boots without crashing, NOT so apps share a "dev" namespace by
accident.

### MongoDB

DON'T:

```ts
const db = client.db();              // empty → "test" DB or worse
const db = client.db("appdb");       // shared across every app
```

DO:

```ts
const dbName = process.env.AGENTRY_APP_NAME ?? "dev";
const db = client.db(dbName);
// optional: assert at boot that we got a real name
if (process.env.NODE_ENV === "production" && dbName === "dev") {
  throw new Error("AGENTRY_APP_NAME must be set in production");
}
```

### Postgres / MySQL

Use a per-app schema. Create it at boot, then set `search_path`:

```ts
const ns = process.env.AGENTRY_APP_NAME ?? "dev";
// Run once at boot. CREATE IF NOT EXISTS is idempotent + safe to
// re-run, so you don't need a separate migration step for it.
await sql`CREATE SCHEMA IF NOT EXISTS ${sql(ns)}`;
await sql`SET search_path TO ${sql(ns)}, public`;
// Every CREATE TABLE / SELECT now lands inside the per-app schema.
```

If you can't make schemas work (some managed Postgres tiers restrict
DDL), prefix table names instead: `${ns}_users`, `${ns}_sessions`.

### Redis

Prefix every key with the app namespace. A tiny helper keeps it
unmissable at the call site:

```ts
const ns = process.env.AGENTRY_APP_NAME ?? "dev";
const k = (suffix: string) => `${ns}:${suffix}`;

await redis.set(k(`session:${id}`), token);
await redis.zadd(k("leaderboard"), score, userId);
```

### S3 / object storage

Every object key gets the app prefix; the bucket is shared:

```ts
const prefix = process.env.AGENTRY_APP_NAME ?? "dev";
await s3.send(new PutObjectCommand({
  Bucket: process.env.AWS_S3_BUCKET!,
  Key: `${prefix}/uploads/${file}`,
  Body,
}));
```

When listing, scope to the prefix:

```ts
const out = await s3.send(new ListObjectsV2Command({
  Bucket, Prefix: `${prefix}/`,
}));
```

### ClickHouse

Same idea as Mongo: pick the DATABASE explicitly.

```ts
const database = process.env.AGENTRY_APP_NAME ?? "dev";
const client = createClient({ url: process.env.CLICKHOUSE_URL!, database });
// CREATE DATABASE IF NOT EXISTS at boot, then create tables inside it.
```

### Exception: services that already namespace by API key

Stripe, OpenAI, Anthropic, SMTP — the user's KEY is the namespace.
Two apps sharing the same Stripe key share the same customer ledger,
so apps using these services should ask the user for a SEPARATE key
per app rather than trying to multiplex one key across many apps.
