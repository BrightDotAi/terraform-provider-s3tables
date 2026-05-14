# BrightAI Terraform Provider

Terraform provider for managing AWS S3 Tables Iceberg resources via the Glue catalog.

Registry address: `registry.terraform.io/anthonykosky/terraform-provider-test`

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
- [Go](https://golang.org/doc/install) >= 1.24 (for building from source)
- AWS credentials configured (environment variables, `~/.aws/credentials`, or IAM role)

## Provider Configuration

```hcl
terraform {
  required_providers {
    brightai = {
      source  = "anthonykosky/terraform-provider-test"
      version = "~> 0.1"
    }
  }
}

provider "brightai" {
  region = "us-east-1"  # optional — falls back to AWS_REGION / AWS_DEFAULT_REGION
}
```

### Provider Arguments

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `region` | string | No | AWS region. Defaults to `AWS_REGION` or `AWS_DEFAULT_REGION` environment variables. |

The provider uses the standard AWS credential chain: environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`), shared credentials file, EC2/ECS instance profiles, or IAM roles.

## Resources

### `brightai_s3_table`

Manages an [Apache Iceberg](https://iceberg.apache.org/) table in an S3 Tables bucket via the AWS Glue catalog.

Schema columns and partition fields can be added or removed without destroying and recreating the table. Renaming a column or changing its type requires removing the old column and adding a new one (in-place rename/retype is not supported by the Iceberg spec).

#### Example — Basic table

```hcl
resource "brightai_s3_table" "events" {
  table_bucket_arn = "arn:aws:s3tables:us-east-1:123456789012:bucket/my-table-bucket"
  namespace        = "analytics"
  name             = "events"

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
    name = "payload"
    type = "string"
  }
}
```

#### Example — Table with time-based and identity partitions

```hcl
resource "brightai_s3_table" "events_partitioned" {
  table_bucket_arn = "arn:aws:s3tables:us-east-1:123456789012:bucket/my-table-bucket"
  namespace        = "analytics"
  name             = "events_partitioned"

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
resource "brightai_s3_table" "metrics" {
  table_bucket_arn = "arn:aws:s3tables:us-east-1:123456789012:bucket/my-table-bucket"
  namespace        = "monitoring"
  name             = "metrics"

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

#### Argument Reference

**Top-level arguments:**

| Argument | Type | Required | Forces New Resource | Description |
|----------|------|----------|---------------------|-------------|
| `table_bucket_arn` | string | Yes | Yes | ARN of the S3 table bucket (`arn:aws:s3tables:{region}:{account}:bucket/{name}`). |
| `namespace` | string | Yes | Yes | Glue database name (namespace) that contains the table. |
| `name` | string | Yes | Yes | Name of the table. |

**`field` block** (list — columns can be added or removed without recreating the table):

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `name` | string | Yes | Column name. |
| `type` | string | Yes | Iceberg column type (see [Supported Types](#supported-types)). |
| `required` | bool | No | Whether the column is non-nullable. Defaults to `false`. |
| `doc` | string | No | Documentation string for the column. Defaults to `""`. |

**`partition` block** (list — partition fields can be added or removed without recreating the table):

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `source_name` | string | Yes | Name of the source column to partition by. Must match a `field.name`. |
| `transform` | string | Yes | Partition transform (see [Partition Transforms](#partition-transforms)). |
| `name` | string | Yes | Name for this partition field. |

#### Attribute Reference

In addition to the arguments above, the following attributes are exported:

| Attribute | Type | Description |
|-----------|------|-------------|
| `warehouse_location` | string | S3 URI of the table's warehouse location (e.g. `s3://my-table-bucket/analytics/events`). |
| `metadata_location` | string | S3 URI of the current Iceberg metadata JSON file. Updated on every schema or partition change. |

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

Import an existing table using `table_bucket_arn,namespace,name`:

```shell
terraform import brightai_s3_table.events \
  "arn:aws:s3tables:us-east-1:123456789012:bucket/my-table-bucket,analytics,events"
```

## Building the Provider

```shell
git clone https://github.com/anthonykosky/terraform-provider-test
cd terraform-provider-test
make install   # builds and installs binary to $GOPATH/bin
```

To use a locally built provider without publishing to the registry, add a `dev_overrides` block to `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "anthonykosky/terraform-provider-test" = "/path/to/your/GOPATH/bin"
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
