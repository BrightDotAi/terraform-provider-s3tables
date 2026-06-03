# Import format: <principal_arn>,<region>,<catalog_id>
#
# <catalog_id> has the form <account_id>:s3tablescatalog/<bucket_name>

terraform import bai_lakeformation_permissions.table_reader \
  "arn:aws:iam::123456789012:role/read-only-role,us-east-1,123456789012:s3tablescatalog/my-table-bucket"
