// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccS3TableResource(t *testing.T) {
	warehouse := "123456789012:s3tablescatalog/test-bucket"
	region := "us-east-1"
	namespace := "test_namespace"
	name := "test_table"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccS3TableResourceConfig(warehouse, region, namespace, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("brightai_s3_table.test", "warehouse", warehouse),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "region", region),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "namespace", namespace),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "name", name),
				),
			},
			{
				ResourceName:      "brightai_s3_table.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateId:     fmt.Sprintf("%s,%s,%s,%s", warehouse, region, namespace, name),
			},
		},
	})
}

func testAccS3TableResourceConfig(warehouse, region, namespace, name string) string {
	return fmt.Sprintf(`
resource "brightai_s3_table" "test" {
  warehouse = %[1]q
  region    = %[2]q
  namespace = %[3]q
  name      = %[4]q
}
`, warehouse, region, namespace, name)
}
