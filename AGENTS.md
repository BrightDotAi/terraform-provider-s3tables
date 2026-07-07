# AGENTS.md

## Cursor Cloud specific instructions

This repository is a **Terraform provider** (`terraform-provider-s3tables`) written in Go. It has no
long-running services — "running the application" means building the provider binary and driving it
through the Terraform CLI via `dev_overrides`.

### Toolchain / prerequisites (already provisioned in the VM snapshot)
- Go: `go.mod` pins the version and the `go` command auto-downloads the matching toolchain.
- `golangci-lint` is installed at `$(go env GOPATH)/bin` (added to `PATH` via `~/.bashrc`).
- `terraform` is installed at `/usr/local/bin/terraform`.
- `~/.terraformrc` contains a `dev_overrides` block mapping `BrightDotAi/s3tables` →
  `/home/ubuntu/go/bin` so a locally built provider is used without a registry.

### Standard commands (see `GNUmakefile` / `README.md` "Developing the Provider")
- Build: `go build -v ./...` or `make build`
- Install the provider binary to `$(go env GOPATH)/bin`: `make install`
- Lint: `make lint` (`golangci-lint run`) — CI runs `golangci-lint` at `latest`.
- Unit tests: `make test` (CI runs `go test -v -cover ./internal/provider/`).
- Regenerate docs: `make generate` (needs `terraform` on `PATH`; must produce no git diff).

### Non-obvious gotchas
- Acceptance tests (`make testacc`, `TF_ACC=1`, tests named `TestAcc*`) create **real AWS resources**
  and require live AWS credentials. Do not run them without credentials — the default `make test`
  runs unit tests only.
- The provider source address is `BrightDotAi/s3tables` (matches `main.go` and
  `examples/provider/provider.tf`). The `dev_overrides` example in `README.md` uses a different
  string (`BrightDotAi/brightai-s3tables`); use `BrightDotAi/s3tables` to match the actual config.
- The `bai_lakeformation_permissions` resource models `permissions` / `grantable_permissions` as
  **nested attributes** (assign with `permissions = { ... }`), while `catalog`, `database`, and
  `table` are **blocks** (`catalog { ... }`). Several README examples show `permissions { ... }`
  block syntax, which Terraform rejects — use the `=` assignment form.
- You can run the provider end-to-end **without AWS credentials**: `terraform plan` on a *create*
  config only exercises the schema, `ValidateResource`, and `ModifyPlan` (no AWS API calls), so it
  is a safe way to smoke-test provider logic. Actual create/read/update/delete require AWS access.
