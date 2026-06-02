---
page_title: "bai_lakeformation_permissions Resource - bai"
subcategory: ""
description: |-
  Manages AWS Lake Formation permissions for a catalog, databases, and tables.
---

# bai_lakeformation_permissions (Resource)

Grants and revokes AWS Lake Formation permissions for an IAM principal over a catalog, its databases, and their tables.

Each `permissions` and `grantable_permissions` block accepts either the shorthand `all = true` **or** individual boolean flags — never both. Setting `all = true` is equivalent to setting every individual flag and causes the provider to send `ALL` to the API. If all individual flags happen to be `true` at grant time the provider also collapses them to `ALL` automatically.

**Omit vs empty block semantics:** These rules apply on both update and destroy.

- If a `permissions` or `grantable_permissions` block is **omitted**, the provider makes no API call for that field and leaves the currently active permissions unchanged.
- If a block is **explicitly present but empty** (e.g. `permissions {}`), the provider treats this as "no permissions" and revokes any currently granted permissions for that field.
- A `databases`, `tables`, or `wildcard` entry **absent from the configuration** is fully revoked on update or destroy.
- A `databases` entry **present in the configuration but with no `permissions` block** leaves that database's permissions untouched on update and skips any revoke call on destroy.

On destroy, only permissions that were explicitly declared in state are revoked. Catalog and database levels with no permissions block will not generate any revoke call.

Each `tables` block and each `wildcard` block must include at least one of `permissions` or `grantable_permissions` (or both). Catalog and database blocks have no such requirement and may omit both.

The `wildcard` block grants table-level permissions across every table in a database; it is mutually exclusive with named `tables` blocks within the same database entry.

Changing `principal`, `region`, or `catalog.id` destroys and recreates the resource.

## Example Usage

### Catalog-level — subset of permissions

```hcl
resource "bai_lakeformation_permissions" "catalog_subset" {
  principal = "arn:aws:iam::123456789012:role/DataEngineer"
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

### Catalog-level — all permissions via `all`

```hcl
resource "bai_lakeformation_permissions" "catalog_all" {
  principal = "arn:aws:iam::123456789012:role/DataAdmin"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    permissions {
      all = true
    }
  }
}
```

### Database-level — subset of permissions

```hcl
resource "bai_lakeformation_permissions" "database_subset" {
  principal = "arn:aws:iam::123456789012:role/Analyst"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    databases {
      name = "analytics"

      permissions {
        describe = true
      }
    }
  }
}
```

### Database-level — all permissions via `all`, with grant option

```hcl
resource "bai_lakeformation_permissions" "database_all" {
  principal = "arn:aws:iam::123456789012:role/DatabaseOwner"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    databases {
      name = "analytics"

      permissions {
        all = true
      }

      grantable_permissions {
        all = true
      }
    }
  }
}
```

### Named table — subset of permissions

```hcl
resource "bai_lakeformation_permissions" "named_tables_subset" {
  principal = "arn:aws:iam::123456789012:role/Analyst"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    databases {
      name = "analytics"

      tables {
        name = "events"

        permissions {
          select   = true
          describe = true
        }
      }

      tables {
        name = "users"

        permissions {
          select = true
          insert = true
        }
      }
    }
  }
}
```

### Named table — all permissions via `all`

```hcl
resource "bai_lakeformation_permissions" "named_table_all" {
  principal = "arn:aws:iam::123456789012:role/TableOwner"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    databases {
      name = "analytics"

      tables {
        name = "events"

        permissions {
          all = true
        }
      }
    }
  }
}
```

### Wildcard — subset of permissions

Grants permissions on all current and future tables in the database.

```hcl
resource "bai_lakeformation_permissions" "wildcard_subset" {
  principal = "arn:aws:iam::123456789012:role/Analyst"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    databases {
      name = "analytics"

      wildcard {
        permissions {
          select   = true
          describe = true
        }
      }
    }
  }
}
```

### Wildcard — all permissions via `all`

```hcl
resource "bai_lakeformation_permissions" "wildcard_all" {
  principal = "arn:aws:iam::123456789012:role/DatabaseOwner"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    databases {
      name = "analytics"

      wildcard {
        permissions {
          all = true
        }
      }
    }
  }
}
```

### Table permissions only — catalog and database permissions unchanged

Omitting `permissions` and `grantable_permissions` at the catalog and database levels leaves those permissions untouched on update and skips any revoke call for those levels on destroy. Only the named table permissions are managed by this resource.

```hcl
resource "bai_lakeformation_permissions" "table_only" {
  principal = "arn:aws:iam::123456789012:role/Analyst"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    # No catalog-level permissions block — existing catalog permissions are preserved.

    databases {
      name = "analytics"

      # No database-level permissions block — existing database permissions are preserved.

      tables {
        name = "events"

        permissions {
          select   = true
          describe = true
        }
      }
    }
  }
}
```

### Full example — catalog, named tables, wildcard, mixed `all` and subsets

```hcl
resource "bai_lakeformation_permissions" "full" {
  principal = "arn:aws:iam::123456789012:role/DataPlatform"
  region    = "us-east-1"

  catalog {
    id = "123456789012"

    # Catalog-level: allow creating new databases
    permissions {
      create_database = true
    }

    # Database with named table permissions
    databases {
      name = "analytics"

      permissions {
        create_table = true
        describe     = true
      }

      tables {
        name = "events"

        permissions {
          select = true
          insert = true
        }

        grantable_permissions {
          select = true
        }
      }

      tables {
        name = "users"

        permissions {
          select   = true
          describe = true
        }
      }
    }

    # Database with wildcard — subset of table permissions
    databases {
      name = "raw"

      permissions {
        describe = true
      }

      wildcard {
        permissions {
          select = true
        }
      }
    }

    # Database with full (ALL) permissions via wildcard
    databases {
      name = "staging"

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

## Schema

### Required

- `principal` (String) IAM principal ARN to grant permissions to. Changing this forces a new resource.
- `region` (String) AWS region where the Lake Formation permissions reside. Changing this forces a new resource.

### Blocks

- `catalog` (Block, Required) Catalog-level permissions and nested database/table permissions. (see [below for nested schema](#nestedblock--catalog))

---

<a id="nestedblock--catalog"></a>
### Nested Schema for `catalog`

#### Required

- `id` (String) AWS account ID (catalog ID) that owns the resources. Changing this forces a new resource.

#### Optional

- `permissions` (Attributes) Catalog-level permissions to grant. (see [below for nested schema](#nestedblock--catalog--permissions))
- `grantable_permissions` (Attributes) Catalog-level permissions the principal can grant to others. (see [below for nested schema](#nestedblock--catalog--permissions))
- `databases` (Block List) Database-level permissions. (see [below for nested schema](#nestedblock--catalog--databases))

<a id="nestedblock--catalog--permissions"></a>
### Nested Schema for `catalog.permissions` / `catalog.grantable_permissions`

All fields are optional. `all` is mutually exclusive with every other field in this block.

- `all` (Boolean) Grants all catalog permissions. Cannot be set alongside individual permission attributes.
- `alter` (Boolean) Grants `ALTER` on the catalog.
- `create_catalog` (Boolean) Grants `CREATE_CATALOG`.
- `create_database` (Boolean) Grants `CREATE_DATABASE`.
- `describe` (Boolean) Grants `DESCRIBE` on the catalog.
- `drop` (Boolean) Grants `DROP` on the catalog.

When `all = true`, or when all five individual flags are `true`, the provider sends `ALL` to the API.

---

<a id="nestedblock--catalog--databases"></a>
### Nested Schema for `catalog.databases`

#### Required

- `name` (String) Database name.

#### Optional

- `permissions` (Attributes) Database-level permissions to grant. (see [below for nested schema](#nestedblock--catalog--databases--permissions))
- `grantable_permissions` (Attributes) Database-level permissions the principal can grant to others. (see [below for nested schema](#nestedblock--catalog--databases--permissions))
- `tables` (Block List) Named table permissions. Mutually exclusive with `wildcard`. Each entry must specify at least one of `permissions` or `grantable_permissions`. (see [below for nested schema](#nestedblock--catalog--databases--tables))
- `wildcard` (Block) Permissions on all tables in this database. Mutually exclusive with `tables`. Must specify at least one of `permissions` or `grantable_permissions`. (see [below for nested schema](#nestedblock--catalog--databases--wildcard))

<a id="nestedblock--catalog--databases--permissions"></a>
### Nested Schema for `catalog.databases.permissions` / `catalog.databases.grantable_permissions`

All fields are optional. `all` is mutually exclusive with every other field in this block.

- `all` (Boolean) Grants all database permissions. Cannot be set alongside individual permission attributes.
- `alter` (Boolean) Grants `ALTER` on the database.
- `create_table` (Boolean) Grants `CREATE_TABLE`.
- `describe` (Boolean) Grants `DESCRIBE` on the database.
- `drop` (Boolean) Grants `DROP` on the database.

When `all = true`, or when all four individual flags are `true`, the provider sends `ALL` to the API.

---

<a id="nestedblock--catalog--databases--tables"></a>
### Nested Schema for `catalog.databases.tables`

#### Required

- `name` (String) Table name.

#### At least one required

- `permissions` (Attributes) Table-level permissions to grant. (see [below for nested schema](#nestedblock--catalog--databases--tables--permissions))
- `grantable_permissions` (Attributes) Table-level permissions the principal can grant to others. (see [below for nested schema](#nestedblock--catalog--databases--tables--permissions))

<a id="nestedblock--catalog--databases--tables--permissions"></a>
### Nested Schema for `catalog.databases.tables.permissions` / `catalog.databases.tables.grantable_permissions`

All fields are optional. `all` is mutually exclusive with every other field in this block.

- `all` (Boolean) Grants all table permissions. Cannot be set alongside individual permission attributes.
- `alter` (Boolean) Grants `ALTER` on the table.
- `delete` (Boolean) Grants `DELETE` on the table.
- `describe` (Boolean) Grants `DESCRIBE` on the table.
- `drop` (Boolean) Grants `DROP` on the table.
- `insert` (Boolean) Grants `INSERT` on the table.
- `select` (Boolean) Grants `SELECT` on the table.

When `all = true`, or when all six individual flags are `true`, the provider sends `ALL` to the API.

---

<a id="nestedblock--catalog--databases--wildcard"></a>
### Nested Schema for `catalog.databases.wildcard`

#### At least one required

- `permissions` (Attributes) Table-level permissions to grant on all tables. (see [below for nested schema](#nestedblock--catalog--databases--tables--permissions))
- `grantable_permissions` (Attributes) Table-level permissions the principal can grant to others on all tables. (see [below for nested schema](#nestedblock--catalog--databases--tables--permissions))

Uses the same permission fields as [`catalog.databases.tables.permissions`](#nestedblock--catalog--databases--tables--permissions).
