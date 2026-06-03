# BrightAI Terraform Provider

Terraform provider for managing AWS S3 Tables Iceberg resources via the Glue catalog.

Registry address: `registry.terraform.io/BrightDotAi/brightai-s3tables`

## Motivation

This provider is intended to provide resources to overcome limitations of the AWS `aws_s3tables_table`
resource provided by the `aws` and `awscc` providers. Speciffically it will:

- Allow for specificataion of partitions and properties in S3tables table declarations.
- Allow for import of existing s3tables tables, and for updating/evolving schemas for s3tables tables 
without causing the table to be destroyed and recreated.

### Known limitations

- The resource makes use of AWS glue RESTful interface, and ther
[iceberg-go](https://pkg.go.dev/github.com/apache/iceberg-go) and is subject
to the current limitations of that package (see <https://go.iceberg.apache.org/>).
- If using Lakeformation permissions to manage access to S3Tables, you will need to ensure that
the role being used by Terraform has `ADD`, `DELETE`, `ALTER` and `DESCRIBE` on the S3tables table.
If importing an existing S3Tables table, `DESCRIBE` permissions will also be needed on the S3tables
catalog.
- Currently default values are supported for types `boolean`, `int`, `long`, `float`, `double` and `string` only. 
(In addition, adding default values requires use of icebert v3 format.)

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
- [Go](https://golang.org/doc/install) >= 1.24 (for building from source)
- AWS credentials configured (environment variables, `~/.aws/credentials`, or IAM role)

## Provider Configuration

```hcl
terraform {
  required_providers {
    bai = {
      source  = "BrightDotAi/s3tables"
      version = "~> 1.0"
    }
  }
}

provider "bai" {
  region  = "us-east-1"  # optional — falls back to AWS_REGION / AWS_DEFAULT_REGION
  profile = "my-profile" # optional — falls back to AWS_PROFILE
}
```

### Provider Arguments

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `region` | string | No | AWS region. Defaults to `AWS_REGION` or `AWS_DEFAULT_REGION` environment variables. |
| `profile` | string | No | AWS named profile from `~/.aws/credentials` or `~/.aws/config`. Defaults to `AWS_PROFILE` environment variable. |

The provider uses the standard AWS credential chain: environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`), shared credentials/config file (optionally selecting a named profile with `profile`), EC2/ECS instance profiles, or IAM roles.

## Resources

### `bai_s3tables_table`

Manages an [Apache Iceberg](https://iceberg.apache.org/) table in an S3 Tables bucket via the AWS Glue catalog.

Schema columns and partition fields can be added or removed without destroying and recreating the table. Renaming a column or changing its type requires removing the old column and adding a new one (in-place rename/retype is not supported by the Iceberg spec).

> **Note:** Table properties cannot be updated after creation. To change properties, destroy and recreate the resource.

#### Example — Basic table

```hcl
resource "bai_s3tables_table" "events" {
  warehouse = "123456789012:s3tablescatalog/my-table-bucket"
  region    = "us-east-1"
  namespace = "analytics"
  name      = "events"

  field {
    name     = "event_id"
    type     = "string"
    required = true
    doc      = "Unique event identifier"
  }

  field {
    name = "event_type"
    type = "string"
  }

  field {
    name = "occurred_at"
    type = "timestamptz"
  }

  field {
    name    = "payload"
    type    = "string"
    default = ""
  }
}
```

#### Example — Table with time-based and identity partitions

```hcl
resource "bai_s3tables_table" "events_partitioned" {
  warehouse = "123456789012:s3tablescatalog/my-table-bucket"
  region    = "us-east-1"
  namespace = "analytics"
  name      = "events_partitioned"

  field {
    name     = "event_id"
    type     = "string"
    required = true
  }

  field {
    name = "event_type"
    type = "string"
  }

  field {
    name = "occurred_at"
    type = "timestamptz"
  }

  field {
    name = "amount"
    type = "decimal(18,4)"
  }

  field {
    name = "region_code"
    type = "string"
  }

  # Partition by year and month derived from occurred_at
  partition {
    source_name = "occurred_at"
    transform   = "year"
    name        = "year"
  }

  partition {
    source_name = "occurred_at"
    transform   = "month"
    name        = "month"
  }

  # Partition by exact value of region_code
  partition {
    source_name = "region_code"
    transform   = "identity"
    name        = "region_code"
  }
}
```

#### Example — Bucket and truncate partitioning

```hcl
resource "bai_s3tables_table" "metrics" {
  warehouse = "123456789012:s3tablescatalog/my-table-bucket"
  region    = "us-east-1"
  namespace = "monitoring"
  name      = "metrics"

  field {
    name     = "metric_id"
    type     = "long"
    required = true
  }

  field {
    name = "service_name"
    type = "string"
  }

  field {
    name = "recorded_at"
    type = "timestamp"
  }

  field {
    name = "value"
    type = "double"
  }

  # Hash metric_id into 64 buckets
  partition {
    source_name = "metric_id"
    transform   = "bucket[64]"
    name        = "metric_bucket"
  }

  # Partition by day
  partition {
    source_name = "recorded_at"
    transform   = "day"
    name        = "day"
  }

  # Truncate service_name to first 4 characters
  partition {
    source_name = "service_name"
    transform   = "truncate[4]"
    name        = "service_prefix"
  }
}
```

#### Example — Table with properties

```hcl
resource "bai_s3tables_table" "events" {
  warehouse = "123456789012:s3tablescatalog/my-table-bucket"
  region    = "us-east-1"
  namespace = "analytics"
  name      = "events"

  field {
    name     = "event_id"
    type     = "string"
    required = true
  }

  property {
    name  = "write.metadata.compression-codec"
    value = "gzip"
  }

  property {
    name  = "write.target-file-size-bytes"
    value = "134217728"
  }
}
```

#### Argument Reference

**Top-level arguments:**

| Argument | Type | Required | Forces New Resource | Description |
|----------|------|----------|---------------------|-------------|
| `warehouse` | string | Yes | Yes | Warehouse identifier: `{account}:s3tablescatalog/{bucket-name}`. |
| `region` | string | Yes | Yes | AWS region where the table bucket resides (e.g. `us-east-1`). |
| `namespace` | string | Yes | Yes | Glue database name (namespace) that contains the table. |
| `name` | string | Yes | Yes | Name of the table. |
| `format_version` | string | No | Yes | Iceberg format version. Accepted values: `"2"` (default) or `"3"`. Version 3 is required to use column default values. |

**`field` block** (list — columns can be added or removed without recreating the table):

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `name` | string | Yes | Column name. |
| `type` | string | Yes | Iceberg column type (see [Supported Types](#supported-types)). |
| `required` | bool | No | Whether the column is non-nullable. Defaults to `false`. |
| `default_string` | string | No | Default string value for `string` columns. At most one `default_*` attribute may be set per field. |
| `default_number` | number | No | Default numeric value for `int`, `long`, `float`, or `double` columns. At most one `default_*` attribute may be set per field. |
| `default_bool` | bool | No | Default boolean value for `boolean` columns. At most one `default_*` attribute may be set per field. |
| `doc` | string | No | Documentation string for the column. Defaults to `""`. |

**`partition` block** (list — partition fields can be added or removed without recreating the table):

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `source_name` | string | Yes | Name of the source column to partition by. Must match a `field.name`. |
| `transform` | string | Yes | Partition transform (see [Partition Transforms](#partition-transforms)). |
| `name` | string | Yes | Name for this partition field. |

**`property` block** (list — Iceberg table properties set at creation time):

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `name` | string | Yes | Property key (e.g. `write.metadata.compression-codec`). |
| `value` | string | Yes | Property value. |

> **Note:** Properties cannot be changed after the table is created. Any modification to a `property` block will cause an error. To change properties, recreate the resource.

#### Supported Types

| Type | Description |
|------|-------------|
| `boolean` | True/false |
| `int` | 32-bit integer |
| `long` | 64-bit integer |
| `float` | 32-bit IEEE 754 float |
| `double` | 64-bit IEEE 754 float |
| `date` | Calendar date (no time zone) |
| `time` | Time of day (no date, no time zone) |
| `timestamp` | Timestamp without time zone |
| `timestamptz` | Timestamp with time zone (UTC) |
| `string` | UTF-8 string |
| `binary` | Arbitrary byte array |
| `uuid` | UUID |
| `fixed[N]` | Fixed-length byte array of length N (e.g. `fixed[16]`) |
| `decimal(P,S)` | Decimal with precision P and scale S (e.g. `decimal(18,4)`) |

#### Partition Transforms

| Transform | Applicable types | Description |
|-----------|-----------------|-------------|
| `identity` | Any | Partition by exact column value. |
| `year` | `date`, `timestamp`, `timestamptz` | Extract year. |
| `month` | `date`, `timestamp`, `timestamptz` | Extract year and month. |
| `day` | `date`, `timestamp`, `timestamptz` | Extract year, month, and day. |
| `hour` | `timestamp`, `timestamptz` | Extract year, month, day, and hour. |
| `bucket[N]` | integer, long, string, uuid, binary, decimal | Hash into N buckets (e.g. `bucket[64]`). |
| `truncate[N]` | int, long, string, binary, decimal | Truncate to width N (e.g. `truncate[4]`). |

#### Import

Import an existing table using `warehouse,region,namespace,name`:

```shell
terraform import bai_s3tables_table.events \
  "123456789012:s3tablescatalog/my-table-bucket,us-east-1,analytics,events"
```

### `bai_lakeformation_permissions`

Manages AWS Lake Formation permissions for an IAM principal over a catalog, its databases, and their tables. On each apply, the provider revokes any permissions that have been removed and grants any new ones — only touching resources explicitly declared in the configuration.

#### Example — Catalog-level permissions

```hcl
resource "bai_lakeformation_permissions" "data_admin" {
  principal = "arn:aws:iam::123456789012:role/DataAdminRole"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    permissions {
      create_database = true
      describe        = true
    }
  }
}
```

#### Example — Database-level permissions

```hcl
resource "bai_lakeformation_permissions" "db_user" {
  principal = "arn:aws:iam::123456789012:role/AnalyticsRole"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    database {
      name = "analytics"

      permissions {
        describe     = true
        create_table = true
      }
    }
  }
}
```

#### Example — Named table permissions

```hcl
resource "bai_lakeformation_permissions" "table_reader" {
  principal = "arn:aws:iam::123456789012:role/ReaderRole"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    database {
      name = "analytics"

      table {
        name = "events"

        permissions {
          select   = true
          describe = true
        }
      }

      table {
        name = "metrics"

        permissions {
          select = true
          insert = true
        }
      }
    }
  }
}
```

#### Example — Wildcard permissions (all tables in a database)

```hcl
resource "bai_lakeformation_permissions" "db_full_access" {
  principal = "arn:aws:iam::123456789012:role/ETLRole"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    database {
      name = "analytics"

      wildcard {
        permissions {
          select = true
          insert = true
          delete = true
          alter  = true
        }
      }
    }
  }
}
```

#### Example — Using `all = true` shorthand

Set `all = true` to grant all permissions for that resource level at once, instead of listing individual flags. This is equivalent to specifying every individual permission as `true`.

```hcl
resource "bai_lakeformation_permissions" "full_access" {
  principal = "arn:aws:iam::123456789012:role/AdminRole"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    permissions {
      all = true
    }

    database {
      name = "analytics"

      permissions {
        all = true
      }

      wildcard {
        permissions {
          all = true
        }
      }
    }
  }
}
```

#### Example — Grantable permissions (grant with grant option)

`grantable_permissions` grants the principal the ability to re-grant those same permissions to other principals. It mirrors `permissions` in structure and may be configured alongside or independently of `permissions`.

```hcl
resource "bai_lakeformation_permissions" "delegated_admin" {
  principal = "arn:aws:iam::123456789012:role/DelegatedAdminRole"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    database {
      name = "analytics"

      table {
        name = "events"

        permissions {
          select   = true
          describe = true
        }

        grantable_permissions {
          select = true
        }
      }
    }
  }
}
```

#### Omitted vs empty permissions blocks

The `permissions` and `grantable_permissions` blocks have two distinct states that behave differently:

- **Omitted** (`permissions` block absent): The provider makes no Lake Formation API call for that permission type. Existing permissions are left untouched, regardless of what AWS currently has. Use this when you want Terraform to manage only a subset of a principal's permissions.

- **Empty** (`permissions {}` block present but no flags set): The provider actively revokes all permissions of that type for the resource. Use this to explicitly remove all previously granted permissions.

```hcl
# permissions block omitted — provider ignores existing catalog permissions
catalog {
  id = "123456789012"

  database {
    name = "analytics"

    permissions {
      describe = true   # only this is managed; grantable_permissions are untouched
    }
  }
}
```

```hcl
# empty permissions block — provider revokes all database permissions
catalog {
  id = "123456789012"

  database {
    name = "analytics"

    permissions {}   # revokes any previously granted database permissions
  }
}
```

Similarly, removing a `database`, `table`, or `wildcard` block from configuration causes the provider to revoke all associated permissions on the next apply.

#### Argument Reference

**Top-level arguments:**

| Argument | Type | Required | Forces New Resource | Description |
|----------|------|----------|---------------------|-------------|
| `principal` | string | Yes | Yes | IAM principal ARN to grant permissions to. |
| `region` | string | Yes | Yes | AWS region where the Lake Formation permissions reside. |

**`catalog` block:**

| Argument | Type | Required | Forces New Resource | Description |
|----------|------|----------|---------------------|-------------|
| `id` | string | Yes | Yes | AWS account ID (catalog ID) that owns the resources. |
| `permissions` | block | No | — | Catalog-level permissions to grant (see below). |
| `grantable_permissions` | block | No | — | Catalog-level permissions the principal can grant to others. |
| `database` | block list | No | — | Zero or more database blocks. |

**`database` block:**

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `name` | string | Yes | Database name. |
| `permissions` | block | No | Database-level permissions to grant. |
| `grantable_permissions` | block | No | Database-level permissions the principal can grant to others. |
| `table` | block list | No | Named table permission blocks. Mutually exclusive with `wildcard`. |
| `wildcard` | block | No | Permissions on all tables in this database. Mutually exclusive with `table`. |

**`table` block:**

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `name` | string | Yes | Table name. |
| `permissions` | block | No | Table-level permissions to grant. |
| `grantable_permissions` | block | No | Table-level permissions the principal can grant to others. |

**`wildcard` block:**

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `permissions` | block | No | Table-level permissions to grant on all tables in the database. |
| `grantable_permissions` | block | No | Table-level permissions the principal can grant to others on all tables. |

**Catalog `permissions` / `grantable_permissions` attributes:**

| Attribute | Description |
|-----------|-------------|
| `all` | Grant all catalog permissions. Mutually exclusive with individual attributes. |
| `alter` | `ALTER` |
| `create_catalog` | `CREATE_CATALOG` |
| `create_database` | `CREATE_DATABASE` |
| `describe` | `DESCRIBE` |
| `drop` | `DROP` |

**Database `permissions` / `grantable_permissions` attributes:**

| Attribute | Description |
|-----------|-------------|
| `all` | Grant all database permissions. Mutually exclusive with individual attributes. |
| `alter` | `ALTER` |
| `create_table` | `CREATE_TABLE` |
| `describe` | `DESCRIBE` |
| `drop` | `DROP` |

**Table `permissions` / `grantable_permissions` attributes (also applies to `wildcard`):**

| Attribute | Description |
|-----------|-------------|
| `all` | Grant all table permissions. Mutually exclusive with individual attributes. |
| `alter` | `ALTER` |
| `delete` | `DELETE` |
| `describe` | `DESCRIBE` |
| `drop` | `DROP` |
| `insert` | `INSERT` |
| `select` | `SELECT` |

## Building the Provider

```shell
git clone https://github.com/BrightDotAi/terraform-provider-brightai-s3tables
cd terraform-provider-brightai-s3tables
make install   # builds and installs binary to $GOPATH/bin
```

To use a locally built provider without publishing to the registry, add a `dev_overrides` block to `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "BrightDotAi/brightai-s3tables" = "/path/to/your/GOPATH/bin"
  }
  direct {}
}
```

## Developing the Provider

```shell
make build      # compile
make test       # unit tests
make testacc    # acceptance tests (creates real AWS resources — costs money)
make lint       # run golangci-lint
make fmt        # run gofmt
make generate   # regenerate /docs/ from /examples/ and schema descriptions
```

Run a single acceptance test:

```shell
go test -v -run TestAccS3TableResource ./internal/provider/
```

Acceptance tests require valid AWS credentials and set `TF_ACC=1` automatically via `make testacc`.
