# Using the MongoDB backend

[MongoDB](https://www.mongodb.com) is a document-oriented database that provides high availability, horizontal scaling,
and rich query capabilities.

The MongoDB backend stores each revision event (create, update, or delete) as a document in a `kine` collection and uses
a separate `revision` collection as an atomic counter for global ordering.

Key characteristics of this backend:

- Atomic revision counter using MongoDB `$inc` operations
- Polling-based watch mechanism (no Change Streams required)
- TTL support for leased keys
- Multiple kine instances can safely share the same MongoDB deployment (leader election enabled)
- Requires MongoDB >= 4.4

## Configuring KINE

This is done by specifying the `--endpoint` option with the following format:

```
mongodb://[<user>:<password>@]<host>[:<port>][/<database>]
```

or using the DNS seed list format for replica sets and MongoDB Atlas:

```
mongodb+srv://[<user>:<password>@]<host>[/<database>]
```

The tokens are defined as follows:

- `user` / `password` - Optional credentials for authentication.
- `host` - Hostname or IP address of the MongoDB server. Default is `localhost`.
- `port` - Port of the MongoDB server. Default is `27017`.
- `database` - Name of the MongoDB database to use. Default is `kine`.

Any additional parameters supported by
the [MongoDB connection string](https://www.mongodb.com/docs/manual/reference/connection-string/) can be appended as
query parameters (e.g. `authSource`, `replicaSet`, `tls`).

### Examples

Connect to a local MongoDB instance using the default database `kine`:

```
mongodb://localhost:27017
```

Connect specifying a custom database name:

```
mongodb://localhost:27017/mydb
```

Connect with authentication:

```
mongodb://admin:secret@localhost:27017/kine
```

Connect to a MongoDB Atlas cluster using the DNS seed list format:

```
mongodb+srv://user:password@cluster0.example.mongodb.net/kine
```

Connect to a replica set:

```
mongodb://mongo1:27017,mongo2:27017,mongo3:27017/kine?replicaSet=rs0
```

## Running kine standalone

Start kine pointing to a local MongoDB instance:

```bash
kine --endpoint "mongodb://localhost:27017/kine"
```

With authentication:

```bash
kine --endpoint "mongodb://admin:secret@localhost:27017/kine"
```

With TLS:

```bash
kine --endpoint "mongodb://localhost:27017/kine" \
  --ca-file ca.crt \
  --cert-file client.crt \
  --key-file client.key
```

## Using with k3s

```bash
k3s server --datastore-endpoint "mongodb://localhost:27017/kine"
```

With TLS:

```bash
k3s server \
  --datastore-endpoint "mongodb://localhost:27017/kine" \
  --datastore-cafile ca.crt \
  --datastore-certfile client.crt \
  --datastore-keyfile client.key
```

## MongoDB schema

### Collection: `kine`

Each document represents one revision event for a key.

| Field            | Type     | Description                                 |
|------------------|----------|---------------------------------------------|
| `_id`            | ObjectID | MongoDB internal document ID                |
| `revision`       | int64    | Unique monotonic global revision            |
| `key`            | string   | Key path                                    |
| `created`        | int64    | `1` if this document is a create event      |
| `deleted`        | int64    | `1` if this document is a delete tombstone  |
| `createRevision` | int64    | Revision at which the key was first created |
| `prevRevision`   | int64    | Previous revision for this key              |
| `lease`          | int64    | Lease ID (used for TTL expiry)              |
| `value`          | bytes    | Value payload                               |
| `prevValue`      | bytes    | Previous value (populated on update events) |
| `version`        | int64    | Version counter for this key                |

### Collection: `revision`

A single document used as the global atomic revision counter.

| Field             | Type     | Description                                                 |
|-------------------|----------|-------------------------------------------------------------|
| `_id`             | ObjectID | Document ID                                                 |
| `revision`        | int64    | Current global revision (incremented atomically via `$inc`) |
| `compactRevision` | int64    | Last compacted revision                                     |

### Indexes on `kine`

| Index               | Fields           | Notes                    |
|---------------------|------------------|--------------------------|
| `idx_revision`      | `revision`       | Unique — global ordering |
| `idx_key`           | `key`            | Key lookups              |
| `idx_key_id`        | `key`, `_id`     | Range queries            |
| `idx_id_deleted`    | `_id`, `deleted` | Soft-delete filtering    |
| `idx_prev_revision` | `prevRevision`   | Watch / After queries    |

## Local development with Docker Compose and k3d

The file [docker-compose.mongodb.yml](docker-compose.mongodb.yml) provides a ready-to-use stack with MongoDB and kine
built from the local source. Combined with [k3d](https://k3d.io), you can run a full k3s cluster locally without
installing anything on the host.

### Prerequisites

- [Docker](https://docs.docker.com/engine/install/)
- [k3d](https://k3d.io/stable/#install-script)

```bash
curl -s https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | bash
```

### Step 1 — Start MongoDB and kine

Run from the repository root:

```bash
docker compose -f examples/docker-compose.mongodb.yml up -d --build
```

Wait for kine to be ready:

```bash
docker compose -f examples/docker-compose.mongodb.yml logs -f kine
```

You should see kine listening on `0.0.0.0:2379`.

### Step 2 — Create the k3s cluster

k3d must join the same Docker network (`kine-net`) so that its containers can reach the `kine` service by name:

```bash
k3d cluster create dev \
  --network kine-net \
  --k3s-arg "--datastore-endpoint=http://kine:2379@server:*" \
  --k3s-arg "--disable=local-storage@server:*" \
  --no-lb
```

Verify the cluster is up:

```bash
kubectl get nodes
kubectl get pods -A
```

### Step 3 — Tear everything down

```bash
k3d cluster delete dev
docker compose -f examples/docker-compose.mongodb.yml down -v
```

The `-v` flag also removes the MongoDB data volume.

## Migrating from another backend

> **Warning:** the migration tool is experimental and has not been extensively tested across different cluster sizes,
> workloads, or failure scenarios. Use it at your own risk. Always take a full backup of your source database before
> proceeding.

kine does not support `etcdctl snapshot` (the etcd binary snapshot format is not implemented). Migration between
backends is done via the etcd KV API using the tool at [migrate/main.go](migrate/main.go).

The tool reads all keys from the source kine endpoint in paginated batches and writes them to the target. Binary
values (protobuf-encoded Kubernetes objects) are preserved correctly.

### Step 1 — Run both kine instances simultaneously

Start the source (PostgreSQL) kine on its default port and the target (MongoDB) kine on a different port:

```bash
# source — existing PostgreSQL kine (already running, e.g. on :2379)

# target — MongoDB kine on a different port
kine --endpoint "mongodb://localhost:27017/kine" --listen-address 0.0.0.0:2380
```

### Step 2 — Stop k3s / k8s writes

Before migrating, stop the API server so no new writes reach the source during the copy:

```bash
# k3s
systemctl stop k3s

# or k3d
k3d cluster stop <cluster-name>
```

### Step 3 — Run the migration

```bash
go run ./examples/migrate \
  -source http://localhost:2379 \
  -target http://localhost:2380
```

Example output:

```
source: http://localhost:2379
target: http://localhost:2380
starting migration...
  3842 keys migrated
migration complete: 3842 keys restored
```

### Step 4 — Switch k3s to the MongoDB kine

Update the k3s datastore endpoint to point to the new kine instance and restart:

```bash
# Edit /etc/systemd/system/k3s.service or pass the flag directly
k3s server --datastore-endpoint "mongodb://localhost:27017/kine"
```

### What is preserved

| Item                      | Preserved                            |
|---------------------------|--------------------------------------|
| Current value of each key | Yes                                  |
| Revision history          | No — revisions are reassigned from 1 |
| Leases / TTLs             | No                                   |

Revision history is not needed for normal Kubernetes operation. The API server rebuilds its watch cache from the current
state on startup.
