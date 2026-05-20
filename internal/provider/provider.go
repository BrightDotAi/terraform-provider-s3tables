// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure BrightAIProvider satisfies various provider interfaces.
var _ provider.Provider = &BrightAIProvider{}

// BrightAIProvider defines the provider implementation.
type BrightAIProvider struct {
	// version is set to the provider version on release, "dev" when the
	// provider is built and ran locally, and "test" when running acceptance
	// testing.
	version string
}

// BrightAIProviderModel describes the provider data model.
type BrightAIProviderModel struct {
	Region types.String `tfsdk:"region"`
	Profile types.String `tfsdk:"profile"`
}

func (p *BrightAIProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "brightai"
	resp.Version = p.version
}

func (p *BrightAIProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"region": schema.StringAttribute{
				MarkdownDescription: "AWS region. Defaults to the value of the `AWS_REGION` / `AWS_DEFAULT_REGION` environment variables.",
				Optional:            true,
			},
			"profile": schema.StringAttribute{
				MarkdownDescription: "AWS IAM Profile. Defaults to the value of the `AWS_PROFILE` environment variable.",
				Optional:            true,
			},
		},
	}
}

func (p *BrightAIProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data BrightAIProviderModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	cfg, d := loadAWSConfig(ctx, data)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.DataSourceData = cfg
	resp.ResourceData = cfg
}

func loadAWSConfig(ctx context.Context, data BrightAIProviderModel) (aws.Config, diag.Diagnostics) {
	var diags diag.Diagnostics

	opts := []func(*awsconfig.LoadOptions) error{}
	if !data.Region.IsNull() && !data.Region.IsUnknown() {
		opts = append(opts, awsconfig.WithRegion(data.Region.ValueString()))
	}
	if !data.Profile.IsNull() && !data.Profile.IsUnknown() {
		opts = append(opts, awsconfig.WithSharedConfigProfile(data.Profile.ValueString()))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		diags.AddError("Failed to load AWS configuration", err.Error())
		return aws.Config{}, diags
	}

	return cfg, diags
}

func (p *BrightAIProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewS3TableResource,
	}
}

func (p *BrightAIProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &BrightAIProvider{
			version: version,
		}
	}
}
