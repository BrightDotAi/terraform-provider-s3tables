
#### Example — Basic table

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

#### Example — Table with time-based and identity partitions

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

#### Example — Bucket and truncate partitioning

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

#### Example — Table with properties

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
