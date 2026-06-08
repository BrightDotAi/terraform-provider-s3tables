
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
