---
page_title: "BrightAI Provider"
description: |-
  
---
# BrightAI Terraform Provider

Terraform provider for managing AWS S3 Tables Iceberg resources via the Glue catalog.

Registry address: `registry.terraform.io/BrightDotAi/brightai-s3tables`

## Motivation

This provider is intended to provide resources to overcome limitations of the AWS `aws_s3tables_table`
resource provided by the `aws` and `awscc` providers. Speciffically it will:

- Allow for specificataion of partitions and properties in S3tables table declarations.
- Allow for import of existing s3tables tables, and for updating/evolving schemas for s3tables tables 
without causing the table to be destroyed and recreated.


## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
- [Go](https://golang.org/doc/install) >= 1.24 (for building from source)
- AWS credentials configured (environment variables, `~/.aws/credentials`, or IAM role)

## Provider Configuration

```hcl
terraform {
  required_providers {
    bai = {
      source  = "BrightDotAi/brightai-s3tables"
      version = "~> 0.1"
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


