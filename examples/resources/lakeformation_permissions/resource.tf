
#### Example — Catalog-level permissions

resource "bai_lakeformation_permissions" "catalog_admin" {
  principal = "arn:aws:iam::123456789012:role/analytics-role"
  region    = "us-east-1"

  catalog {
    id = "123456789012:s3tablescatalog/my-table-bucket"

    permissions {
      create_database = true
      describe        = true
    }
  }
}

#### Example — Database-level permissions

resource "bai_lakeformation_permissions" "db_access" {
  principal = "arn:aws:iam::123456789012:role/analytics-role"
  region    = "us-east-1"

  catalog {
    id = "123456789012:s3tablescatalog/my-table-bucket"

    database {
      name = "analytics"

      permissions {
        describe     = true
        create_table = true
      }
    }
  }
}

#### Example — Named table permissions

resource "bai_lakeformation_permissions" "table_reader" {
  principal = "arn:aws:iam::123456789012:role/read-only-role"
  region    = "us-east-1"

  catalog {
    id = "123456789012:s3tablescatalog/my-table-bucket"

    database {
      name = "analytics"

      table {
        name = "events"

        permissions {
          select   = true
          describe = true
        }
      }
    }
  }
}

#### Example — All tables in a database (wildcard)

resource "bai_lakeformation_permissions" "db_wildcard" {
  principal = "arn:aws:iam::123456789012:role/etl-role"
  region    = "us-east-1"

  catalog {
    id = "123456789012:s3tablescatalog/my-table-bucket"

    database {
      name = "analytics"

      wildcard {
        permissions {
          select = true
          insert = true
          delete = true
        }
      }
    }
  }
}

#### Example — Grantable permissions (grant option)

resource "bai_lakeformation_permissions" "db_admin_with_grant" {
  principal = "arn:aws:iam::123456789012:role/data-steward-role"
  region    = "us-east-1"

  catalog {
    id = "123456789012:s3tablescatalog/my-table-bucket"

    database {
      name = "analytics"

      permissions {
        all = true
      }

      grantable_permissions {
        describe     = true
        create_table = true
      }
    }
  }
}

#### Example — Multi-database, multi-table permissions

resource "bai_lakeformation_permissions" "full_access" {
  principal = "arn:aws:iam::123456789012:role/platform-role"
  region    = "us-east-1"

  catalog {
    id = "123456789012:s3tablescatalog/my-table-bucket"

    permissions {
      create_database = true
      describe        = true
    }

    database {
      name = "analytics"

      permissions {
        all = true
      }

      table {
        name = "events"

        permissions {
          all = true
        }
      }

      table {
        name = "metrics"

        permissions {
          select   = true
          describe = true
        }
      }
    }

    database {
      name = "raw"

      permissions {
        describe     = true
        create_table = true
      }

      wildcard {
        permissions {
          select = true
        }
      }
    }
  }
}
